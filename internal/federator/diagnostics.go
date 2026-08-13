package federator

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// AgentStatus is the live status returned by an agent's local control socket.
type AgentStatus struct {
	RuntimeVersion  string   `json:"runtime_version"`
	ProtocolVersion int      `json:"protocol_version"`
	HostID          string   `json:"host_id"`
	HostName        string   `json:"host_name"`
	Hub             string   `json:"hub"`
	Connected       bool     `json:"connected"`
	LocalPeers      int      `json:"local_peers"`
	RemotePeers     int      `json:"remote_peers"`
	RemoteHosts     int      `json:"remote_hosts"`
	Capabilities    []string `json:"capabilities,omitempty"`
	Shadows         int      `json:"shadows"`
	RegistryDir     string   `json:"registry_dir"`
	RuntimeDir      string   `json:"runtime_dir"`
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
	FederatedShadows         int          `json:"federated_shadows"`
	AgentRunning             bool         `json:"agent_running"`
	Agent                    *AgentStatus `json:"agent,omitempty"`
}

// Doctor checks hub compatibility, the local registry, and an optional running agent.
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
		report.UnmessageableLiveRecords, report.FederatedShadows = inspectRegistry(report.RegistryDir)
	if status, err := ReadAgentStatus(options.RuntimeDir); err == nil {
		report.AgentRunning = true
		report.Agent = &status
	}
	report.OK = report.HubCompatible && report.RegistryReady && report.UnmessageableLiveRecords == 0
	switch {
	case options.Hub == "":
		report.Summary = "hub address is not configured"
	case !report.HubReachable:
		report.Summary = "hub is unreachable"
	case !report.HubCompatible:
		report.Summary = "hub protocol is incompatible"
	case !report.RegistryReady:
		report.Summary = "claude session registry is unavailable"
	case report.UnmessageableLiveRecords > 0:
		report.Summary = "live sessions without messaging sockets were found"
	default:
		report.Summary = "ready"
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

func inspectRegistry(registryDir string) (ready bool, live, messageable, unmessageable, shadows int) {
	entries, err := os.ReadDir(registryDir)
	if err != nil {
		return false, 0, 0, 0, 0
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
			shadows++
			continue
		}
		live++
		if record.MessagingSocketPath != "" && probeUnix(record.MessagingSocketPath, 100*time.Millisecond) {
			messageable++
		} else {
			unmessageable++
		}
	}
	return true, live, messageable, unmessageable, shadows
}
