package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type hookInput struct {
	Event          string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

func codexHookThreadID(paths nativePaths, input hookInput) (string, error) {
	sessionID := strings.TrimSpace(input.SessionID)
	if !validSessionID(sessionID) {
		return "", errors.New("codex hook input did not include a valid session_id")
	}
	transcriptPath := strings.TrimSpace(input.TranscriptPath)
	if transcriptPath == "" {
		return sessionID, nil
	}
	resolvedTranscript, err := filepath.EvalSymlinks(transcriptPath)
	if err != nil {
		return "", err
	}
	resolvedSessions, err := filepath.EvalSymlinks(filepath.Join(paths.codexHome, "sessions"))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(resolvedSessions, resolvedTranscript)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("codex hook transcript is outside the configured session store")
	}
	threadID, metadataSessionID, err := readCodexHookSessionMeta(resolvedTranscript)
	if err != nil {
		return "", err
	}
	if metadataSessionID == "" {
		metadataSessionID = threadID
	}
	if threadID != metadataSessionID || metadataSessionID != sessionID {
		return "", errors.New("codex hook transcript identity does not match the hook")
	}
	return threadID, nil
}

func readCodexHookSessionMeta(transcriptPath string) (string, string, error) {
	file, err := os.Open(transcriptPath) //nolint:gosec // product-provided transcript below CODEX_HOME.
	if err != nil {
		return "", "", err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
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
		return strings.TrimSpace(record.Payload.ID), strings.TrimSpace(record.Payload.SessionID), nil
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}
	return "", "", errors.New("codex hook transcript is empty")
}
