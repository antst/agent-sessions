package bridge

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

func qwenTranscriptPath(home, sessionID string) (string, bool) {
	if !filepath.IsAbs(home) || !validSessionID(sessionID) {
		return "", false
	}
	projects := filepath.Join(home, "projects")
	entries, err := os.ReadDir(projects)
	if err != nil {
		return "", false
	}
	selected := ""
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(projects, entry.Name(), "chats", sessionID+".jsonl")
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || selected != "" {
			return "", false
		}
		selected = path
	}
	return selected, selected != ""
}

// QwenNativeSessionTitle returns the latest product-owned manual title for one
// exact transcript. It is a live projection and is never written to disk by
// Agent Sessions.
func QwenNativeSessionTitle(home, sessionID, cwd string) (string, bool) {
	path, ok := qwenTranscriptPath(home, sessionID)
	if !ok {
		return "", false
	}
	file, err := os.Open(path) //nolint:gosec // exact UUID below the selected Qwen profile.
	if err != nil {
		return "", false
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	first, latest := true, ""
	for scanner.Scan() {
		var event struct {
			SessionID     string `json:"sessionId"`
			Cwd           string `json:"cwd"`
			Type          string `json:"type"`
			Subtype       string `json:"subtype"`
			SystemPayload struct {
				CustomTitle string `json:"customTitle"`
				TitleSource string `json:"titleSource"`
			} `json:"systemPayload"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.SessionID != sessionID {
			return "", false
		}
		if first {
			first = false
			if filepath.Clean(event.Cwd) != filepath.Clean(cwd) {
				return "", false
			}
		}
		if event.Type == "system" && event.Subtype == "custom_title" &&
			strings.TrimSpace(event.SystemPayload.TitleSource) != "auto" {
			latest = strings.TrimSpace(event.SystemPayload.CustomTitle)
			if latest == "" || len(latest) > 512 {
				return "", false
			}
		}
	}
	return latest, scanner.Err() == nil
}

// QwenNativeSessionInfo confirms one exact transcript without requiring the
// daemon to remember its cwd.
func QwenNativeSessionInfo(home, sessionID string) (string, string, bool) {
	path, ok := qwenTranscriptPath(home, sessionID)
	if !ok {
		return "", "", false
	}
	file, err := os.Open(path) //nolint:gosec // exact UUID below Qwen's selected profile.
	if err != nil {
		return "", "", false
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	first, cwd, title := true, "", ""
	for scanner.Scan() {
		var event struct {
			SessionID     string `json:"sessionId"`
			Cwd           string `json:"cwd"`
			Type          string `json:"type"`
			Subtype       string `json:"subtype"`
			SystemPayload struct {
				CustomTitle string `json:"customTitle"`
				TitleSource string `json:"titleSource"`
			} `json:"systemPayload"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.SessionID != sessionID {
			return "", "", false
		}
		if first {
			first, cwd = false, event.Cwd
		}
		if event.Type == "system" && event.Subtype == "custom_title" &&
			strings.TrimSpace(event.SystemPayload.TitleSource) != "auto" {
			title = strings.TrimSpace(event.SystemPayload.CustomTitle)
		}
	}
	if scanner.Err() != nil || first {
		return "", "", false
	}
	if title == "" {
		title = sessionID
	}
	return title, cwd, true
}
