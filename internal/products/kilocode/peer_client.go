package kilocode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/productserver"
)

// TUIClient owns Kilo's explicit peer-only routes. These methods must never be
// substituted with /api/session/{id}/prompt, which does not target an attached
// full TUI at the pinned version.
type TUIClient struct {
	http      *productserver.Client
	directory string
	now       func() time.Time
	mu        sync.Mutex
}

func NewTUIClient(client *productserver.Client, directory string, now func() time.Time) (*TUIClient, error) {
	if client == nil || strings.TrimSpace(directory) != directory || directory == "" ||
		len(directory) > 4096 || !utf8.ValidString(directory) || strings.ContainsRune(directory, '\x00') {
		return nil, productruntime.ErrProtocol
	}
	if now == nil {
		now = time.Now
	}
	return &TUIClient{http: client, directory: directory, now: now}, nil
}

type peerMessage struct {
	Info struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
		Role      string `json:"role"`
	} `json:"info"`
	Parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts"`
}

func (client *TUIClient) Deliver(ctx context.Context, nativeSessionID string, body []byte) (productruntime.NativeAcceptance, error) {
	if !validID(nativeSessionID, "ses_") || len(body) == 0 || len(body) > 1<<20 || !utf8.Valid(body) {
		return productruntime.NativeAcceptance{}, productruntime.ErrProtocol
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	before, err := client.messages(ctx, nativeSessionID)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	known := make(map[string]bool, len(before))
	for _, message := range before {
		known[message.Info.ID] = true
	}
	if _, err := client.do(ctx, http.MethodPost, "/tui/clear-prompt", nil, http.StatusOK); err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	if _, err := client.do(ctx, http.MethodPost, "/tui/append-prompt", map[string]string{"text": string(body)}, http.StatusOK); err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	if _, err := client.do(ctx, http.MethodPost, "/tui/submit-prompt", nil, http.StatusOK); err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		messages, queryErr := client.messages(ctx, nativeSessionID)
		if queryErr == nil {
			for _, message := range messages {
				if known[message.Info.ID] || message.Info.SessionID != nativeSessionID || message.Info.Role != "user" || !validID(message.Info.ID, "msg_") {
					continue
				}
				for _, part := range message.Parts {
					if part.Type == "text" && bytes.Equal([]byte(part.Text), body) {
						return productruntime.NativeAcceptance{NativeSessionID: nativeSessionID, NativeMessageID: message.Info.ID, AcceptedAt: client.now().UTC()}, nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return productruntime.NativeAcceptance{}, productruntime.ErrAmbiguousSession
			}
			return productruntime.NativeAcceptance{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

type BackgroundProcess struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	PID       int    `json:"pid"`
	Cwd       string `json:"cwd"`
	Status    string `json:"status"`
}

func (client *TUIClient) AttributeBackgroundProcess(ctx context.Context, pid int, nativeSessionID string) (BackgroundProcess, error) {
	if pid <= 1 || !validID(nativeSessionID, "ses_") {
		return BackgroundProcess{}, productruntime.ErrUnauthorized
	}
	response, err := client.do(ctx, http.MethodGet, "/background-process", nil, http.StatusOK)
	if err != nil {
		return BackgroundProcess{}, err
	}
	var processes []BackgroundProcess
	if json.Unmarshal(response, &processes) != nil || len(processes) > 4096 {
		return BackgroundProcess{}, productruntime.ErrProtocol
	}
	var match *BackgroundProcess
	for index := range processes {
		process := &processes[index]
		if process.PID != pid {
			continue
		}
		if match != nil || process.SessionID != nativeSessionID || process.Cwd != client.directory ||
			!validID(process.ID, "bgp_") || process.Status != "starting" && process.Status != "running" && process.Status != "ready" {
			return BackgroundProcess{}, productruntime.ErrAmbiguousSession
		}
		match = process
	}
	if match == nil {
		return BackgroundProcess{}, productruntime.ErrUnauthorized
	}
	return *match, nil
}

func (client *TUIClient) RegisterMCP(ctx context.Context, name string, command []string) error {
	if !validToken(name) || len(command) == 0 || len(command) > 128 {
		return productruntime.ErrProtocol
	}
	for _, argument := range command {
		if argument == "" || len(argument) > 4096 || strings.ContainsRune(argument, '\x00') {
			return productruntime.ErrProtocol
		}
	}
	body := map[string]any{"name": name, "config": map[string]any{"type": "local", "command": command, "enabled": true, "timeout": 5000}}
	response, err := client.do(ctx, http.MethodPost, "/mcp", body, http.StatusOK)
	if err != nil {
		return err
	}
	var statuses map[string]struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(response, &statuses) != nil || statuses[name].Status != "connected" {
		return productruntime.ErrNativeRejected
	}
	return nil
}

func (client *TUIClient) messages(ctx context.Context, nativeSessionID string) ([]peerMessage, error) {
	response, err := client.do(ctx, http.MethodGet, "/session/"+url.PathEscape(nativeSessionID)+"/message", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var messages []peerMessage
	if json.Unmarshal(response, &messages) != nil || len(messages) > 65536 {
		return nil, productruntime.ErrProtocol
	}
	return messages, nil
}

func (client *TUIClient) do(ctx context.Context, method, route string, body any, expected int) ([]byte, error) {
	query := url.Values{"directory": []string{client.directory}}
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, productruntime.ErrProtocol
		}
	}
	header := make(http.Header)
	if body != nil {
		header.Set("Content-Type", "application/json")
	}
	response, err := client.http.Do(ctx, productserver.Request{Method: method, Path: route + "?" + query.Encode(), Header: header, Body: encoded})
	if err != nil {
		return nil, productruntime.ErrUnavailable
	}
	if response.StatusCode != expected {
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, productruntime.ErrUnauthorized
		case http.StatusNotFound:
			return nil, productruntime.ErrStale
		case http.StatusConflict:
			return nil, productruntime.ErrNativeRejected
		default:
			return nil, productruntime.ErrProtocol
		}
	}
	return response.Body, nil
}

func validID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) <= len(prefix) || len(value) > 256 {
		return false
	}
	for _, character := range value[len(prefix):] {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validToken(value string) bool {
	return productcatalog.ValidateToken(value) == nil
}
