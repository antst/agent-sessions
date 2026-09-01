package federation

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

const (
	// ProtocolVersion is the complete, uniform host/hub wire contract. Every
	// participant must send this exact version during the first handshake; the
	// hub does not partially admit or downgrade mismatched participants.
	ProtocolVersion = 4
	// GroupProtocolVersion is the grouped peer projection carried by protocol 4.
	GroupProtocolVersion = 1

	// CapabilityCodexLane advertises remotely executable Codex lanes.
	CapabilityCodexLane = "codex-lane"
	// CapabilityClaudeLane advertises remotely executable Claude lanes.
	CapabilityClaudeLane = "claude-lane"
	// CapabilityGrokLane advertises remotely executable Grok lanes.
	CapabilityGrokLane = "grok-lane"
	// CapabilityQwenLane advertises remotely executable Qwen lanes.
	CapabilityQwenLane = "qwen-lane"

	maxWireBytes            = 2 * 1024 * 1024
	maxLaneInputBytes       = 1024 * 1024
	maxWireCapabilities     = 64
	maxCapabilityBytes      = 4096
	maxHostIDBytes          = 128
	maxHostNameBytes        = 256
	maxBuildBytes           = 256
	maxProductTokenBytes    = 64
	maxPeerIDBytes          = maxHostIDBytes + 1 + maxSessionIDBytes
	maxPeerGlobalIDBytes    = 128
	maxPeerNameBytes        = 256
	maxPeerDisplayNameBytes = 256
	maxPeerStatusBytes      = 64
	maxPeerCwdBytes         = 4096
	maxPeerPermissionBytes  = 128
	maxPeerInstanceIDBytes  = 512
	maxPeerParentIDBytes    = maxPeerIDBytes
	maxPeerGroups           = 128
	maxSnapshotPeers        = 2048
	maxRosterPeers          = 4096
	maxRosterHosts          = 4096
	wireWriteTimeout        = 10 * time.Second
)

// RuntimeVersion is diagnostic build metadata emitted during registration.
// Equal protocol versions interoperate regardless of this value.
var RuntimeVersion = "dev"

// Host is one connected daemon and the remote lane services it exposes.
type Host struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities,omitempty"`
	Generation   uint64   `json:"generation,omitempty"`
	Build        string   `json:"build,omitempty"`
}

// ParentContext is the source-daemon attestation inherited by a remote child
// lane. Product adapters may use the optional native evidence, but the wire
// engine treats it as opaque identity metadata after validating the routing
// fields against the advertised source peer.
type ParentContext struct {
	HostID               string   `json:"host_id"`
	SessionID            string   `json:"session_id"`
	Product              string   `json:"product"`
	InstanceID           string   `json:"instance_id"`
	Groups               []string `json:"groups"`
	AlwaysApprove        bool     `json:"always_approve"`
	AgentRuntimeDir      string   `json:"agent_runtime_dir,omitempty"`
	AdapterPID           int      `json:"adapter_pid,omitempty"`
	AdapterProcStart     string   `json:"adapter_proc_start,omitempty"`
	AdapterStrongStart   string   `json:"adapter_strong_start,omitempty"`
	AdapterSocket        string   `json:"adapter_socket,omitempty"`
	PID                  int      `json:"pid"`
	ProcStart            string   `json:"proc_start"`
	StrongStart          string   `json:"strong_start,omitempty"`
	PermissionMode       string   `json:"permission_mode"`
	QwenCapabilityDigest string   `json:"qwen_capability_digest,omitempty"`
}

// Message is one bounded newline-delimited host/hub protocol frame.
type Message struct {
	Type          string          `json:"type"`
	Version       int             `json:"version,omitempty"`
	Build         string          `json:"build,omitempty"`
	Generation    uint64          `json:"generation,omitempty"`
	HostID        string          `json:"host_id,omitempty"`
	HostName      string          `json:"host_name,omitempty"`
	TargetHostID  string          `json:"target_host_id,omitempty"`
	Capabilities  []string        `json:"capabilities,omitempty"`
	Hosts         []Host          `json:"hosts,omitempty"`
	Peers         []Peer          `json:"peers,omitempty"`
	SourceID      string          `json:"source_id,omitempty"`
	TargetID      string          `json:"target_id,omitempty"`
	RequestID     string          `json:"request_id,omitempty"`
	Product       string          `json:"product,omitempty"`
	Args          []string        `json:"args,omitempty"`
	Input         []byte          `json:"input,omitempty"`
	Data          []byte          `json:"data,omitempty"`
	ExitCode      int             `json:"exit_code,omitempty"`
	ParentContext *ParentContext  `json:"parent_context,omitempty"`
	Frame         json.RawMessage `json:"frame,omitempty"`
	Error         string          `json:"error,omitempty"`
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

// Send writes one bounded protocol frame to the connection.
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
	if !validSimpleID(message.HostID) || len(message.HostID) > maxHostIDBytes {
		return errors.New("hello requires a simple host_id")
	}
	if strings.TrimSpace(message.HostName) == "" || len(message.HostName) > maxHostNameBytes {
		return errors.New("hello requires host_name")
	}
	if len(message.Build) > maxBuildBytes {
		return errors.New("hello build exceeds protocol bounds")
	}
	return nil
}

func normalizeCapabilities(values []string) ([]string, error) {
	if len(values) > maxWireCapabilities {
		return nil, fmt.Errorf("federation capabilities exceed raw count limit %d", maxWireCapabilities)
	}
	total := 0
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		total += len(value)
		if total > maxCapabilityBytes {
			return nil, fmt.Errorf("federation capabilities exceed aggregate byte limit %d", maxCapabilityBytes)
		}
		if err := productcatalog.ValidateToken(value); err != nil {
			return nil, fmt.Errorf("invalid federation capability %q: %w", value, err)
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sortStrings(result)
	return result, nil
}

func laneCapabilityForMessage(message Message) (string, error) {
	if err := productcatalog.ValidateToken(message.Product); err != nil {
		return "", fmt.Errorf("invalid remote lane product %q: %w", message.Product, err)
	}
	if len(message.Capabilities) != 1 {
		return "", errors.New("remote lane request requires exactly one capability")
	}
	if err := productcatalog.ValidateToken(message.Capabilities[0]); err != nil {
		return "", fmt.Errorf("invalid remote lane capability %q: %w", message.Capabilities[0], err)
	}
	return message.Capabilities[0], nil
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
