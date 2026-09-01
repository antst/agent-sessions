package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/permissionmode"
)

type hookInput struct {
	Event          string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode"`
}

// codexHookThreadID resolves the exact thread represented by a Codex hook.
// Modern Codex hooks carry a materialized local rollout path. Its first
// session_meta record is the host-owned binding between the hook's session
// family and the exact thread; neither model arguments nor a family id alone
// can grant another thread's peer capability.
func codexHookThreadID(paths nativePaths, input hookInput) (string, error) {
	sessionID := strings.TrimSpace(input.SessionID)
	if !validSessionID(sessionID) {
		return "", errors.New("codex hook input did not include a valid session_id")
	}
	transcriptPath := strings.TrimSpace(input.TranscriptPath)
	if transcriptPath == "" {
		// Compatibility with Codex versions that predate transcript_path. Their
		// hook session_id was the exact root thread id.
		return sessionID, nil
	}

	resolvedTranscript, err := confinedCodexHookTranscript(paths, transcriptPath)
	if err != nil {
		return "", err
	}
	threadID, metadataSessionID, err := readCodexHookSessionMeta(resolvedTranscript)
	if err != nil {
		return "", err
	}
	if metadataSessionID == "" {
		metadataSessionID = threadID
	}
	if !validSessionID(threadID) || !validSessionID(metadataSessionID) || metadataSessionID != sessionID {
		return "", errors.New("codex hook transcript identity does not match the hook")
	}
	return threadID, nil
}

func confinedCodexHookTranscript(paths nativePaths, transcriptPath string) (string, error) {
	resolvedTranscript, err := filepath.EvalSymlinks(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("resolve Codex hook transcript: %w", err)
	}
	resolvedSessions, err := filepath.EvalSymlinks(filepath.Join(paths.codexHome, "sessions"))
	if err != nil {
		return "", fmt.Errorf("resolve Codex sessions directory: %w", err)
	}
	relative, err := filepath.Rel(resolvedSessions, resolvedTranscript)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("codex hook transcript is outside the configured session store")
	}
	return resolvedTranscript, nil
}

func readCodexHookSessionMeta(transcriptPath string) (string, string, error) {
	file, err := os.Open(transcriptPath) //nolint:gosec // caller supplies the canonical path confined beneath CODEX_HOME/sessions.
	if err != nil {
		return "", "", fmt.Errorf("open Codex hook transcript: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", "", errors.New("codex hook transcript is not a regular file")
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), maxFrameBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				ID        string `json:"id"`
				SessionID string `json:"session_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &record) != nil || record.Type != "session_meta" {
			return "", "", errors.New("codex hook transcript does not start with session metadata")
		}
		threadID := strings.TrimSpace(record.Payload.ID)
		metadataSessionID := strings.TrimSpace(record.Payload.SessionID)
		return threadID, metadataSessionID, nil
	}
	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("read Codex hook transcript metadata: %w", err)
	}
	return "", "", errors.New("codex hook transcript is empty")
}

func consumeNativeInbox(paths nativePaths, sessionID string) ([]map[string]any, error) {
	names, pending, err := nativeInboxNames(paths, sessionID)
	if err != nil {
		return nil, err
	}
	messages := []map[string]any{}
	for _, name := range names {
		file := filepath.Join(pending, name)
		if value := readJSONMap(file); value != nil {
			if wakeInboxAlreadyDelivered(paths, sessionID, value) {
				_ = os.Remove(file)
				continue
			}
			messages = append(messages, value)
			markWakeInboxDelivered(paths, sessionID, value)
		}
		_ = os.Remove(file)
	}
	return messages, nil
}

func wakeInboxAlreadyDelivered(paths nativePaths, sessionID string, item map[string]any) bool {
	record := readWakeRecord(paths, sessionID, stringValue(item["id"]))
	return record != nil && (record.State == "delivered" || record.State == "fallback_delivered")
}

func markWakeInboxDelivered(paths nativePaths, sessionID string, item map[string]any) {
	record := readWakeRecord(paths, sessionID, stringValue(item["id"]))
	if record == nil || (record.State != "queueing" && record.State != "queued") {
		return
	}
	record.State = "fallback_delivered"
	record.Delivery = "queued"
	_ = writeWakeRecord(paths, *record)
}

func nativeInboxNames(paths nativePaths, sessionID string) ([]string, string, error) {
	pending := filepath.Join(paths.dataRoot, "sessions", sessionKey(sessionID), "inbox", "pending")
	entries, err := os.ReadDir(pending)
	if os.IsNotExist(err) {
		return nil, pending, nil
	}
	if err != nil {
		return nil, pending, err
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) > 50 {
		names = names[:50]
	}
	return names, pending, nil
}

func formatNativeHookMessages(messages []map[string]any) string {
	blocks := []string{}
	for _, message := range messages {
		if stringValue(message["type"]) == "delivery-status" {
			text := fmt.Sprintf("Delivery status from %s: %s", stringValue(message["from"]), stringValue(message["status"]))
			if reason := stringValue(message["reason"]); reason != "" {
				text += " — " + reason
			}
			blocks = append(blocks, text)
			continue
		}
		sender := stringValue(message["from"])
		if name := stringValue(message["fromName"]); name != "" {
			sender = fmt.Sprintf("%s (%s)", name, sender)
		}
		metadata := []string{}
		for _, pair := range [][2]string{
			{"sender-type", stringValue(message["fromProduct"])}, {"message-id", stringValue(message["id"])},
			{"sent-at", stringValue(message["sentAt"])}, {"received-at", stringValue(message["receivedAt"])},
		} {
			if pair[1] != "" {
				metadata = append(metadata, pair[0]+"="+pair[1])
			}
		}
		text := "Message from " + defaultString(sender, "an unidentified peer") + ":"
		if len(metadata) > 0 {
			text += "\nMessage metadata: " + strings.Join(metadata, ", ")
		}
		text += "\n" + stringValue(message["message"])
		blocks = append(blocks, text)
	}
	return strings.Join(blocks, "\n\n---\n\n")
}

func processHasAncestor(start, ancestor int) bool {
	visited := map[int]bool{}
	pid := start
	for pid > 1 && !visited[pid] {
		if pid == ancestor {
			return true
		}
		visited[pid] = true
		var ok bool
		pid, ok = parentProcessID(pid)
		if !ok {
			return false
		}
	}
	return pid == ancestor
}

func parentProcessID(pid int) (int, bool) {
	return parentProcessIDNative(pid)
}

// peerProcessPermissionMode maps the live peer process to the two permission
// classes recognized by Claude's cross-session inbound gate. It is used only
// for bridge-generated infrastructure notices, never to choose Codex policy.
func peerProcessPermissionMode(pid int, procStart string) (string, error) {
	args, err := readPeerProcessArgs(pid, procStart)
	if err != nil {
		return "", err
	}
	return permissionModeFromArgs(args), nil
}

func readPeerProcessArgs(pid int, procStart string) ([]string, error) {
	return readPeerProcessArgsWithReader(pid, procStart, readProcessArgs, time.Sleep, func(pid int, procStart string) bool {
		return exactProcessIdentityStatus(pid, procStart).Status == processIdentityMatches
	})
}

func readPeerProcessArgsWithReader(
	pid int,
	procStart string,
	readArgs func(int) ([]string, error),
	pause func(time.Duration),
	identityMatches func(int, string) bool,
) ([]string, error) {
	if pid <= 1 || procStart == "" {
		return nil, errors.New("peer process identity is unavailable")
	}
	for attempt := 0; attempt < 3; attempt++ {
		if !identityMatches(pid, procStart) {
			return nil, errors.New("peer process identity no longer matches its registry")
		}
		args, err := readArgs(pid)
		if err == nil {
			if !identityMatches(pid, procStart) {
				return nil, errors.New("peer process identity changed while reading its arguments")
			}
			return args, nil
		}
		if (!errors.Is(err, syscall.EIO) && !errors.Is(err, syscall.EINTR)) || attempt == 2 {
			return nil, err
		}
		pause(10 * time.Millisecond)
	}
	return nil, errors.New("peer process arguments remained unavailable")
}

func permissionModeFromArgs(fields []string) string {
	return permissionmode.FromArgs(fields)
}

func runtimeGOOS() string { return runtime.GOOS }
