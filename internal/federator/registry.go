package federator

import (
	"encoding/json"
	"fmt"
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
}

type localPeer struct {
	Peer
	PID    int
	Socket string
}

//nolint:gocyclo // Discovery deliberately keeps all reject-and-skip validation in one read loop.
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
			// sessionId, so keep the shadow address stable across that rotation.
			instanceIdentity = strconv.Itoa(pid) + "\x00" + strconv.FormatInt(record.StartedAt, 10) +
				"\x00" + filepath.Clean(record.MessagingSocketPath)
		} else {
			instanceIdentity = strconv.Itoa(pid) + "\x00" + instanceIdentity
		}
		permissionMode := peerPermissionMode(pid, record.PermissionMode)
		if record.Entrypoint == "grok" {
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
		result[id] = localPeer{Peer: peer, PID: pid, Socket: record.MessagingSocketPath}
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

func writeShadowRecord(registryDir string, pid int, socket string, peer Peer) (string, error) {
	if pid <= 0 || socket == "" {
		return "", fmt.Errorf("invalid shadow process")
	}
	procStart := processStart(pid)
	if procStart == "" {
		return "", fmt.Errorf("cannot corroborate shadow process identity")
	}
	record := registryRecord{
		PID: pid, SessionID: peer.GlobalID, Cwd: peer.Cwd, Name: peer.DisplayName,
		Status: peer.Status, Entrypoint: peer.Entrypoint, ProcStart: procStart,
		PermissionMode:      peer.PermissionMode,
		MessagingSocketPath: socket, StartedAt: peer.StartedAt,
		Version: "peer-federator/" + RuntimeVersion, PeerProtocol: 1, Kind: "interactive",
		NameSource: "federated", FederatedBy: "peer-federator", FederatedPeerID: peer.ID,
		FederatedHost: peer.HostName,
	}
	path := filepath.Join(registryDir, strconv.Itoa(pid)+".json")
	return path, writeJSONAtomic(path, record)
}

func probeUnix(path string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
