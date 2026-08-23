package federator

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

type registryRecord struct {
	PID                 int    `json:"pid,omitempty"`
	SessionID           string `json:"sessionId"`
	Cwd                 string `json:"cwd,omitempty"`
	Name                string `json:"name"`
	Status              string `json:"status,omitempty"`
	Entrypoint          string `json:"entrypoint,omitempty"`
	PermissionMode      string `json:"permissionMode,omitempty"`
	ProcStart           string `json:"procStart,omitempty"`
	MessagingSocketPath string `json:"messagingSocketPath"`
	StartedAt           int64  `json:"startedAt,omitempty"`
	Version             string `json:"version,omitempty"`
	PeerProtocol        int    `json:"peerProtocol,omitempty"`
	Kind                string `json:"kind,omitempty"`
	NameSource          string `json:"nameSource,omitempty"`
	FederatedBy         string `json:"federatedBy,omitempty"`
	FederatedPeerID     string `json:"federatedPeerId,omitempty"`
	FederatedHost       string `json:"federatedHost,omitempty"`
	AgentService        bool   `json:"agentService,omitempty"`
	GroupProtocol       int    `json:"groupProtocol,omitempty"`
	ParentSessionID     string `json:"parentSessionId,omitempty"`
	UpdatedAt           int64  `json:"updatedAt,omitempty"`
	StatusUpdatedAt     int64  `json:"statusUpdatedAt,omitempty"`
}

type localPeer struct {
	Peer
	PID                  int
	ProcStart            string
	Socket               string
	LifecyclePID         int
	LifecycleProcStart   string
	AdapterStrongStart   string
	LifecycleStrongStart string
	LifecycleRoot        string
	ClaudeConfigRoot     string
	ClaudeKeyBaseline    []ClaudeKeyBaselineEntry
	ClaudeKeyBaselineSet bool
	CleanupDebt          []PeerCleanupDebt
	GroupProtocol        int
	AgentService         bool
}

// PeerCleanupDebt retains typed, retryable ownership work after an ambiguous
// post-prepare failure. Text is diagnostic only; expected identity fields are
// the authority for a later retry.
type PeerCleanupDebt struct {
	Version           int    `json:"version"`
	DebtID            string `json:"debt_id"`
	Revision          string `json:"revision"`
	Product           string `json:"product"`
	OwnerKind         string `json:"owner_kind"`
	OwnerID           string `json:"owner_id"`
	Operation         string `json:"operation"`
	ExpectedPath      string `json:"expected_path,omitempty"`
	ExpectedPID       int    `json:"expected_pid,omitempty"`
	ExpectedStart     string `json:"expected_start,omitempty"`
	ExpectedStrong    string `json:"expected_strong_start,omitempty"`
	ExpectedDigest    string `json:"expected_digest,omitempty"`
	ObservationState  string `json:"observation_state"`
	Attempts          int    `json:"attempts"`
	LastError         string `json:"last_error,omitempty"`
	UpdatedAt         int64  `json:"updated_at"`
	TerminalWhenClean string `json:"terminal_when_clean"`
}

//nolint:gocyclo,unparam // Legacy registry fixture keeps validation in one loop; hostID varies in external use.
func discoverLocalPeers(registryDir, hostID, hostName string) (map[string]localPeer, error) {
	entries, err := os.ReadDir(registryDir)
	if os.IsNotExist(err) {
		return map[string]localPeer{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := map[string]localPeer{}
	for _, entry := range entries {
		pid := parsePID(entry.Name())
		if pid == 0 || !processLive(pid) {
			continue
		}
		// #nosec G304 -- entry.Name came from ReadDir on the configured registry.
		body, readErr := os.ReadFile(filepath.Join(registryDir, entry.Name()))
		if readErr != nil {
			continue
		}
		var record registryRecord
		if json.Unmarshal(body, &record) != nil || record.FederatedBy != "" || record.SessionID == "" || record.MessagingSocketPath == "" {
			continue
		}
		if record.ProcStart != "" && processStart(pid) != "" && record.ProcStart != processStart(pid) {
			continue
		}
		if !probeUnix(record.MessagingSocketPath, 100*time.Millisecond) {
			continue
		}
		id := hostID + "/" + record.SessionID
		instanceIdentity := record.ProcStart
		if instanceIdentity == "" {
			// Some native registry writers omit procStart. Their PID, startedAt,
			// and socket remain process-stable when a live Claude process rotates
			// sessionId, so keep the native adapter identity stable across that rotation.
			instanceIdentity = strconv.Itoa(pid) + "\x00" + strconv.FormatInt(record.StartedAt, 10) +
				"\x00" + filepath.Clean(record.MessagingSocketPath)
		} else {
			instanceIdentity = strconv.Itoa(pid) + "\x00" + instanceIdentity
		}
		permissionMode := peerPermissionMode(pid, record.PermissionMode)
		descriptor, knownProduct := ProductByID(record.Entrypoint)
		if knownProduct && descriptor.DynamicPermission {
			// Some products publish permission changes as runtime state rather
			// than immutable argv. If
			// the live bridge cannot corroborate its current state, over-report
			// privilege instead of silently labelling a yolo session constrained.
			permissionMode = "bypassPermissions"
			if inspected, ok := inspectGrokPermissionMode(record.MessagingSocketPath, pid, record); ok {
				permissionMode = inspected
			}
		}
		peer := Peer{
			ID: id, HostID: hostID, HostName: hostName, SessionID: record.SessionID,
			GlobalID: globalSessionID(hostID, record.SessionID), Name: record.Name,
			DisplayName: qualifiedName(record.Name, hostName), Status: record.Status,
			Cwd: record.Cwd, Entrypoint: record.Entrypoint,
			PermissionMode: permissionMode, StartedAt: record.StartedAt,
			PeerProtocol: record.PeerProtocol,
			InstanceID:   sessionKey(hostID + "\x00" + instanceIdentity),
		}
		result[id] = localPeer{
			Peer: peer, PID: pid, Socket: record.MessagingSocketPath,
			GroupProtocol: record.GroupProtocol, AgentService: record.AgentService,
		}
	}
	return result, nil
}

func peersFromLocal(local map[string]localPeer) []Peer {
	peers := make([]Peer, 0, len(local))
	for _, peer := range local {
		peers = append(peers, peer.Peer)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID < peers[j].ID })
	return peers
}

func probeUnix(path string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
