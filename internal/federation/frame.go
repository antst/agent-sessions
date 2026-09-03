// Package federation owns the product-neutral AgentFrame and group admission
// contracts shared by local daemon delivery and network federation.
package federation

import "time"

const (
	// AgentFrameVersion is the compatible product-neutral body protocol.
	AgentFrameVersion = 1
	// MaxAgentContent is the closed per-message content bound.
	MaxAgentContent = 1024 * 1024
)

// Peer is the routing projection of one live attachment or lane. Native
// product evidence remains owned by its adapter and is deliberately absent.
type Peer struct {
	ID              string   `json:"id"`
	SessionID       string   `json:"session_id"`
	GlobalID        string   `json:"global_session_id,omitempty"`
	Name            string   `json:"name"`
	DisplayName     string   `json:"display_name,omitempty"`
	HostID          string   `json:"host_id,omitempty"`
	HostName        string   `json:"host_name,omitempty"`
	Product         string   `json:"product,omitempty"`
	Entrypoint      string   `json:"entrypoint,omitempty"`
	Status          string   `json:"status,omitempty"`
	Cwd             string   `json:"cwd,omitempty"`
	PermissionMode  string   `json:"permission_mode,omitempty"`
	StartedAt       int64    `json:"started_at,omitempty"`
	PeerProtocol    int      `json:"peer_protocol,omitempty"`
	InstanceID      string   `json:"instance_id,omitempty"`
	Groups          []string `json:"groups"`
	ParentSessionID string   `json:"parent_session_id,omitempty"`
}

// AgentFrame is the complete local or federated routing request/delivery.
type AgentFrame struct {
	Version         int      `json:"version"`
	Type            string   `json:"type"`
	MessageID       string   `json:"message_id"`
	SourceSessionID string   `json:"source_session_id,omitempty"`
	Source          *Peer    `json:"source,omitempty"`
	Targets         []string `json:"targets,omitempty"`
	Group           string   `json:"group,omitempty"`
	Content         string   `json:"content,omitempty"`
	Summary         string   `json:"summary,omitempty"`
	SentAt          string   `json:"sent_at,omitempty"`
}

// DeliveryResult is the destination-visible result for one admitted target.
type DeliveryResult struct {
	Target     string `json:"target"`
	SessionID  string `json:"session_id,omitempty"`
	DeliveryID string `json:"delivery_id,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	Cause      error  `json:"-"`
}

// AgentFrameResult is the product-neutral synchronous routing result.
type AgentFrameResult struct {
	Version    int              `json:"version"`
	Type       string           `json:"type"`
	MessageID  string           `json:"message_id,omitempty"`
	Peers      []Peer           `json:"peers,omitempty"`
	Deliveries []DeliveryResult `json:"deliveries,omitempty"`
	Error      string           `json:"error,omitempty"`
}

// DeliveryFrame constructs the exact frame presented to an admitted target.
func DeliveryFrame(request AgentFrame, source Peer) AgentFrame {
	sentAt := request.SentAt
	if sentAt == "" {
		sentAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	copySource := clonePeer(source)
	return AgentFrame{
		Version: AgentFrameVersion, Type: "delivery", MessageID: request.MessageID,
		SourceSessionID: source.SessionID, Source: &copySource, Content: request.Content,
		Group: request.Group, Summary: request.Summary, SentAt: sentAt,
	}
}

func clonePeer(peer Peer) Peer {
	peer.Groups = append([]string(nil), peer.Groups...)
	return peer
}
