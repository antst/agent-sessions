package federator

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
)

const (
	// ProtocolVersion identifies the compatible hub/agent wire contract.
	ProtocolVersion = federation.ProtocolVersion
	// GroupProtocolVersion identifies the local grouped-routing contract.
	GroupProtocolVersion = federation.GroupProtocolVersion
	maxWireBytes         = federation.MaxFrameBytes
	maxLaneInputBytes    = federation.MaxLaneInputBytes
	wireWriteTimeout     = 10 * time.Second
)

// Host remains a compatibility alias for the shared wire identity.
type Host = federation.Host

// Peer remains a compatibility alias for the shared wire projection.
type Peer = federation.Peer

// Message is one newline-delimited hub, agent, or local-control frame.
type Message struct {
	Type                   string               `json:"type"`
	Version                int                  `json:"version,omitempty"`
	HostID                 string               `json:"host_id,omitempty"`
	HostName               string               `json:"host_name,omitempty"`
	TargetHostID           string               `json:"target_host_id,omitempty"`
	Capabilities           []string             `json:"capabilities,omitempty"`
	Hosts                  []Host               `json:"hosts,omitempty"`
	Peers                  []Peer               `json:"peers,omitempty"`
	SourceID               string               `json:"source_id,omitempty"`
	SourceSessionID        string               `json:"source_session_id,omitempty"`
	SessionID              string               `json:"session_id,omitempty"`
	Name                   string               `json:"name,omitempty"`
	ParentSessionID        string               `json:"parent_session_id,omitempty"`
	ParentHostID           string               `json:"parent_host_id,omitempty"`
	ParentGroups           []string             `json:"parent_groups,omitempty"`
	ParentSpecified        bool                 `json:"parent_specified,omitempty"`
	InheritParentGroups    bool                 `json:"inherit_parent_groups,omitempty"`
	InheritGroupsSpecified bool                 `json:"inherit_groups_specified,omitempty"`
	TargetID               string               `json:"target_id,omitempty"`
	TargetIDs              []string             `json:"target_ids,omitempty"`
	Group                  string               `json:"group,omitempty"`
	Groups                 []string             `json:"groups,omitempty"`
	GroupsSpecified        bool                 `json:"groups_specified,omitempty"`
	AlwaysApprove          bool                 `json:"always_approve,omitempty"`
	AlwaysApproveSpecified bool                 `json:"always_approve_specified,omitempty"`
	QwenSession            *QwenSessionMetadata `json:"qwen_session,omitempty"`
	Preference             *SessionPreferences  `json:"preference,omitempty"`
	Registration           *PeerRegistration    `json:"registration,omitempty"`
	ParentContext          *ParentContext       `json:"parent_context,omitempty"`
	RequestID              string               `json:"request_id,omitempty"`
	Product                string               `json:"product,omitempty"`
	SessionKind            string               `json:"session_kind,omitempty"`
	Args                   []string             `json:"args,omitempty"`
	Input                  []byte               `json:"input,omitempty"`
	Data                   []byte               `json:"data,omitempty"`
	ServicePeerToken       string               `json:"service_peer_token,omitempty"`
	ExitCode               int                  `json:"exit_code,omitempty"`
	Frame                  json.RawMessage      `json:"frame,omitempty"`
	Error                  string               `json:"error,omitempty"`
}

type wireConn struct {
	conn         net.Conn
	mu           sync.Mutex
	writeTimeout time.Duration
}

func newWireConn(conn net.Conn) *wireConn {
	return newWireConnWithTimeout(conn, wireWriteTimeout)
}

func newWireConnWithTimeout(conn net.Conn, timeout time.Duration) *wireConn {
	return &wireConn{conn: conn, writeTimeout: timeout}
}

// Send writes one bounded newline-delimited message.
func (w *wireConn) Send(message Message) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(body) > maxWireBytes {
		return fmt.Errorf("federation frame exceeds %d bytes", maxWireBytes)
	}
	body = append(body, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeTimeout > 0 {
		if err := w.conn.SetWriteDeadline(time.Now().Add(w.writeTimeout)); err != nil {
			return err
		}
	}
	_, err = w.conn.Write(body)
	return err
}

func scanMessages(reader io.Reader, handle func(Message) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxWireBytes)
	for scanner.Scan() {
		var message Message
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return fmt.Errorf("decode federation frame: %w", err)
		}
		if err := handle(message); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

func validateHello(message Message) error {
	if message.Type != "hello" || message.Version != ProtocolVersion {
		return errors.New("first frame must be a compatible hello")
	}
	if cleanID(message.HostID) == "" || message.HostID != cleanID(message.HostID) {
		return errors.New("hello requires a simple host_id")
	}
	if message.HostName == "" {
		return errors.New("hello requires host_name")
	}
	return nil
}
