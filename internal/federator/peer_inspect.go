package federator

import (
	"encoding/json"
	"io"
	"net"
	"time"
)

const peerInspectionTimeout = 100 * time.Millisecond

type peerInspection struct {
	Type           string `json:"type"`
	PID            int    `json:"pid"`
	ProcStart      string `json:"procStart"`
	SessionID      string `json:"sessionId"`
	Entrypoint     string `json:"entrypoint"`
	PermissionMode string `json:"permissionMode"`
}

// inspectGrokPermissionMode trusts a dynamic permission class only when the
// live bridge corroborates every process and session identity from the Grok
// registry row. Failure is intentionally non-fatal: callers retain their
// process-argv fallback for older or temporarily unresponsive bridges.
func inspectGrokPermissionMode(socket string, pid int, record registryRecord) (string, bool) {
	if !eligibleGrokInspection(pid, record) {
		return "", false
	}
	conn, err := net.DialTimeout("unix", socket, peerInspectionTimeout)
	if err != nil {
		return "", false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(peerInspectionTimeout))
	if err := json.NewEncoder(conn).Encode(map[string]any{"type": "control", "action": "inspect"}); err != nil {
		return "", false
	}
	var response peerInspection
	if err := json.NewDecoder(io.LimitReader(conn, 64*1024)).Decode(&response); err != nil {
		return "", false
	}
	if !corroboratedGrokInspection(response, pid, record) {
		return "", false
	}
	return response.PermissionMode, true
}

func eligibleGrokInspection(pid int, record registryRecord) bool {
	return record.PID == pid && record.ProcStart != "" && processStart(pid) == record.ProcStart &&
		record.SessionID != "" && record.Entrypoint == "grok"
}

func corroboratedGrokInspection(response peerInspection, pid int, record registryRecord) bool {
	validMode := response.PermissionMode == "default" || response.PermissionMode == "bypassPermissions"
	return response.Type == "peer_inspection" && response.PID == pid && response.ProcStart == record.ProcStart &&
		response.SessionID == record.SessionID && response.Entrypoint == "grok" && validMode
}
