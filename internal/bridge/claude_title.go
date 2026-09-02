package bridge

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeNativeSessionTitle reads one exact session from Claude's own
// transcript store. It confirms existence and returns Claude's latest title.
func ClaudeNativeSessionTitle(configRoot, sessionID string) (string, bool) {
	if !filepath.IsAbs(configRoot) || !validSessionID(sessionID) {
		return "", false
	}
	projects, err := os.ReadDir(filepath.Join(configRoot, "projects"))
	if err != nil {
		return "", false
	}
	selected := ""
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		path := filepath.Join(configRoot, "projects", project.Name(), sessionID+".jsonl")
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || selected != "" {
			return "", false
		}
		selected = path
	}
	if selected == "" {
		return "", false
	}
	file, err := os.Open(selected) //nolint:gosec // exact UUID below Claude's selected profile.
	if err != nil {
		return "", false
	}
	defer func() { _ = file.Close() }()
	title, seen := "", false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var event struct {
			SessionID   string `json:"sessionId"`
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.SessionID != "" && event.SessionID != sessionID {
			return "", false
		}
		seen = true
		if event.Type == "custom-title" && strings.TrimSpace(event.CustomTitle) != "" {
			title = strings.TrimSpace(event.CustomTitle)
		}
	}
	if scanner.Err() != nil || !seen {
		return "", false
	}
	if title == "" {
		title = sessionID
	}
	return title, true
}
