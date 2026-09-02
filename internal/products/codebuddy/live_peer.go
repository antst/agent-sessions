package codebuddy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/productserver"
)

// ReplyLivePeer asks CodeBuddy's own worker registry which live product
// endpoint owns sessionID, confirms that UUID with the product, and replies.
// The registry is product state; Agent Sessions neither copies nor persists it.
func ReplyLivePeer(ctx context.Context, sessionID, body string) error {
	endpoint, err := livePeerEndpoint(sessionID)
	if err != nil {
		return err
	}
	client, err := NewPeerClient(endpoint, productserver.Limits{})
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	live, err := client.LiveSession(ctx)
	if err != nil {
		return err
	}
	if live.SessionID != sessionID {
		return errors.New("CodeBuddy endpoint no longer owns the live session")
	}
	return client.ReplySession(ctx, sessionID, body)
}

func livePeerEndpoint(sessionID string) (string, error) {
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", err
		}
	}
	entries, err := os.ReadDir(filepath.Join(home, ".codebuddy", "sessions"))
	if err != nil {
		return "", err
	}
	endpoint := ""
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(home, ".codebuddy", "sessions", entry.Name()))
		if readErr != nil {
			continue
		}
		var row struct {
			SessionID string `json:"sessionId"`
			Kind      string `json:"kind"`
			Endpoint  string `json:"endpoint"`
			URL       string `json:"url"`
		}
		if json.Unmarshal(body, &row) != nil || row.SessionID != sessionID || row.Kind != "interactive" {
			continue
		}
		candidate := strings.TrimSpace(row.Endpoint)
		if candidate == "" {
			candidate = strings.TrimSpace(row.URL)
		}
		if candidate == "" || endpoint != "" && endpoint != candidate {
			return "", errors.New("CodeBuddy live session endpoint is ambiguous")
		}
		endpoint = candidate
	}
	if endpoint == "" {
		return "", errors.New("CodeBuddy live session is unavailable")
	}
	return endpoint, nil
}
