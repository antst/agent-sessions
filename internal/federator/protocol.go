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
)

const (
	// ProtocolVersion identifies the compatible hub/agent wire contract.
	ProtocolVersion   = 2
	maxWireBytes      = 2 * 1024 * 1024
	maxLaneInputBytes = 1024 * 1024
	wireWriteTimeout  = 10 * time.Second
)

const (
	// CapabilityCodexLane is the Codex lane host-roster feature identifier.
	CapabilityCodexLane = "codex-lane"
	// CapabilityClaudeLane is the Claude lane host-roster feature identifier.
	CapabilityClaudeLane = "claude-lane"
	// CapabilityGrokLane is the Grok lane host-roster feature identifier.
	CapabilityGrokLane = "grok-lane"
)

// Host is one connected federation agent and the remote services it can run.
type Host struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Peer is one live, messageable session advertised by a host agent.
type Peer struct {
	ID             string `json:"id"`
	HostID         string `json:"host_id"`
	HostName       string `json:"host_name"`
	SessionID      string `json:"session_id"`
	GlobalID       string `json:"global_session_id"`
	Name           string `json:"name"`
	DisplayName    string `json:"display_name"`
	Status         string `json:"status,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	Entrypoint     string `json:"entrypoint,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	StartedAt      int64  `json:"started_at,omitempty"`
	PeerProtocol   int    `json:"peer_protocol,omitempty"`
	InstanceID     string `json:"instance_id,omitempty"`
}

// Message is one newline-delimited hub, agent, or local-control frame.
type Message struct {
	Type            string          `json:"type"`
	Version         int             `json:"version,omitempty"`
	HostID          string          `json:"host_id,omitempty"`
	HostName        string          `json:"host_name,omitempty"`
	TargetHostID    string          `json:"target_host_id,omitempty"`
	Capabilities    []string        `json:"capabilities,omitempty"`
	Hosts           []Host          `json:"hosts,omitempty"`
	Peers           []Peer          `json:"peers,omitempty"`
	SourceID        string          `json:"source_id,omitempty"`
	SourceSessionID string          `json:"source_session_id,omitempty"`
	TargetID        string          `json:"target_id,omitempty"`
	RequestID       string          `json:"request_id,omitempty"`
	Product         string          `json:"product,omitempty"`
	Args            []string        `json:"args,omitempty"`
	Input           []byte          `json:"input,omitempty"`
	Data            []byte          `json:"data,omitempty"`
	ExitCode        int             `json:"exit_code,omitempty"`
	Frame           json.RawMessage `json:"frame,omitempty"`
	Error           string          `json:"error,omitempty"`
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
