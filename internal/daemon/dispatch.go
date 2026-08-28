package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
)

var controlRoleOperations = map[controlRole]map[string]struct{}{
	controlRoleAdmin: operationSet(
		"runtime.status", "runtime.doctor", "remove.inspect",
	),
	controlRoleLauncher: operationSet(
		"attachment.prepare", "attachment.adopt", "attachment.refresh", "attachment.detach", "attachment.lookup",
		"lane.command",
		"lane.start", "lane.resume", "lane.followup", "lane.status", "lane.list",
		"lane.interrupt", "lane.collect", "lane.archive",
	),
	controlRoleHook: operationSet(
		"attachment.adopt", "attachment.refresh", "attachment.detach",
	),
	controlRoleConnector: operationSet(
		"mcp.forward",
		"attachment.context", "peer.identity", "peer.discover", "peer.send", "peer.broadcast", "peer.inbox", "peer.rename",
		"lane.command",
		"lane.start", "lane.resume", "lane.followup", "lane.status", "lane.list",
		"lane.interrupt", "lane.collect", "lane.archive",
	),
	controlRoleService: operationSet("runtime.status", "remove.inspect"),
}

var controlRoleTopics = map[controlRole]map[string]struct{}{
	controlRoleAdmin:     operationSet("runtime.state"),
	controlRoleLauncher:  operationSet("attachment.state", "lane.notice"),
	controlRoleHook:      operationSet("attachment.state"),
	controlRoleConnector: operationSet("lane.notice", "peer.inbox"),
	controlRoleService:   operationSet("runtime.state"),
}

func operationSet(operations ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(operations))
	for _, operation := range operations {
		result[operation] = struct{}{}
	}
	return result
}

func controlRoleAllowsOperation(role controlRole, operation string) bool {
	_, ok := controlRoleOperations[role][operation]
	return ok
}

func controlRoleAllowsTopic(role controlRole, topic string) bool {
	_, ok := controlRoleTopics[role][topic]
	return ok
}

type controlReplayKey struct {
	Principal string `json:"principal"`
	RequestID string `json:"request_id"`
}

type controlReplayEntry struct {
	Operation     string          `json:"operation"`
	PayloadDigest string          `json:"payload_digest"`
	Response      json.RawMessage `json:"response"`
}

type controlReplayStore interface {
	lookup(context.Context, controlReplayKey) (controlReplayEntry, bool, error)
	commit(context.Context, controlReplayKey, controlReplayEntry) error
}

type memoryControlReplayStore struct {
	mu      sync.Mutex
	entries map[controlReplayKey]controlReplayEntry
}

func newMemoryControlReplayStore() *memoryControlReplayStore {
	return &memoryControlReplayStore{entries: make(map[controlReplayKey]controlReplayEntry)}
}

func (store *memoryControlReplayStore) lookup(_ context.Context, key controlReplayKey) (controlReplayEntry, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.entries[key]
	entry.Response = append(json.RawMessage(nil), entry.Response...)
	return entry, ok, nil
}

func (store *memoryControlReplayStore) commit(_ context.Context, key controlReplayKey, entry controlReplayEntry) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry.Response = append(json.RawMessage(nil), entry.Response...)
	store.entries[key] = entry
	return nil
}

func controlPrincipalReplayNamespace(principal controlPrincipal) string {
	body, _ := json.Marshal(struct {
		Role         controlRole `json:"role"`
		Product      string      `json:"product,omitempty"`
		AttachmentID string      `json:"attachment_id,omitempty"`
		SessionID    string      `json:"session_id,omitempty"`
		Attested     bool        `json:"attested"`
		UID          int         `json:"uid"`
		PID          int         `json:"pid"`
		ProcStart    string      `json:"proc_start"`
		StrongStart  string      `json:"strong_start"`
	}{
		Role: principal.Role, Product: principal.Product, AttachmentID: principal.AttachmentID,
		SessionID: principal.SessionID, Attested: principal.Attested, UID: principal.Peer.UID, PID: principal.Peer.PID,
		ProcStart: principal.Peer.ProcStart, StrongStart: principal.Peer.StrongStart,
	})
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func controlPayloadDigest(payload json.RawMessage) (string, error) {
	var value any
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(payload, &value); err != nil {
		return "", fmt.Errorf("decode request payload: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}
