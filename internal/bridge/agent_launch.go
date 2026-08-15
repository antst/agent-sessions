package bridge

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// agentLaunchRecord is the shared process-attested launch boundary for agent
// products whose host supplies a conversation ID only after exec. Product
// adapters own their hook protocol; this record owns token, ancestry, and
// lifecycle identity so the next adapter does not reimplement that policy.
type agentLaunchRecord struct {
	Product         string `json:"product"`
	OwnerPID        int    `json:"ownerPid"`
	OwnerProcStart  string `json:"ownerProcStart"`
	ConversationID  string `json:"conversationId,omitempty"`
	Cwd             string `json:"cwd"`
	Name            string `json:"name,omitempty"`
	PermissionMode  string `json:"permissionMode"`
	StartupInjected bool   `json:"startupInjected,omitempty"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

func authorizedAgentLaunch(paths nativePaths, token, product string, requireAttached bool) (agentLaunchRecord, string, error) {
	return authorizedAgentLaunchForProcess(paths, token, product, requireAttached, os.Getpid())
}

func authorizedAgentLaunchForProcess(paths nativePaths, token, product string, requireAttached bool, processPID int) (agentLaunchRecord, string, error) {
	if len(token) != 64 {
		return agentLaunchRecord{}, "", errors.New("agent launch token is absent")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return agentLaunchRecord{}, "", errors.New("agent launch token is malformed")
	}
	path := agentLaunchRecordPath(paths, token)
	var record agentLaunchRecord
	body, err := os.ReadFile(path) //nolint:gosec // path is derived from a SHA-256 token digest inside private bridge state.
	if err != nil || json.Unmarshal(body, &record) != nil {
		return agentLaunchRecord{}, "", errors.New("agent launch token is unknown")
	}
	if record.Product != product {
		return agentLaunchRecord{}, "", errors.New("agent launch token belongs to another product")
	}
	if !exactProcessIdentityMatch(record.OwnerPID, record.OwnerProcStart) || !processHasAncestor(processPID, record.OwnerPID) {
		return agentLaunchRecord{}, "", errors.New("agent launch owner is not this process's live ancestor")
	}
	if requireAttached && !validSessionID(record.ConversationID) {
		return agentLaunchRecord{}, "", errors.New("agent launch has not attached to a conversation")
	}
	return record, path, nil
}

func liveAgentLaunchForSession(paths nativePaths, sessionID, product string) *agentLaunchRecord {
	if !validSessionID(sessionID) {
		return nil
	}
	directory := filepath.Join(paths.dataRoot, "agent-launches")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var record agentLaunchRecord
		body, readErr := os.ReadFile(filepath.Join(directory, entry.Name())) //nolint:gosec // entry is from the bridge-owned launch directory.
		if readErr == nil && json.Unmarshal(body, &record) == nil && record.Product == product &&
			record.ConversationID == sessionID && exactProcessIdentityMatch(record.OwnerPID, record.OwnerProcStart) {
			return &record
		}
	}
	return nil
}

func agentLaunchRecordPath(paths nativePaths, token string) string {
	digest := sha256.Sum256([]byte(token))
	return filepath.Join(paths.dataRoot, "agent-launches", hex.EncodeToString(digest[:])+".json")
}

func newAgentLaunchToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func lockAgentLaunch(paths nativePaths, recordPath string) (func(), error) {
	directory := filepath.Join(paths.dataRoot, "agent-launches")
	if filepath.Clean(filepath.Dir(recordPath)) != filepath.Clean(directory) {
		return nil, errors.New("agent launch record is outside the private launch directory")
	}
	if err := os.MkdirAll(directory, 0700); err != nil { // #nosec G703 -- resolveNativePaths selects the bridge's private per-user state root.
		return nil, err
	}
	lock, err := os.OpenFile(recordPath+".lock", os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // private bridge state lock.
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}, nil
}

func cleanupStaleAgentLaunches(paths nativePaths) {
	directory := filepath.Join(paths.dataRoot, "agent-launches")
	entries, _ := os.ReadDir(directory)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		var record agentLaunchRecord
		body, err := os.ReadFile(path) //nolint:gosec // entry is from the bridge-owned launch directory.
		if err != nil || json.Unmarshal(body, &record) != nil || normalizePeerProduct(record.Product) == "" ||
			!exactProcessIdentityMatch(record.OwnerPID, record.OwnerProcStart) {
			_ = os.Remove(path)
			_ = os.Remove(path + ".lock")
		}
	}
}
