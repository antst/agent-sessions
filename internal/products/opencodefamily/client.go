// Package opencodefamily contains the verified HTTP/SSE mechanics shared by
// OpenCode and Kilo Code. Product-specific peer routing and lifecycle quirks
// deliberately live in their respective product packages.
package opencodefamily

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/productserver"
)

const (
	maxNativeIDBytes = 256
	maxTitleBytes    = 1024
	maxResultBytes   = 1 << 20
)

type Dialect string

const (
	DialectOpenCode Dialect = "opencode"
	DialectKilo     Dialect = "kilo"
)

type ClientConfig struct {
	HTTP      *productserver.Client
	Directory string
	Dialect   Dialect
	Now       func() time.Time
}

// Client owns product semantics above the bounded literal-loopback mechanics.
// It deliberately has no endpoint or credential fields.
type Client struct {
	http      *productserver.Client
	directory string
	dialect   Dialect
	now       func() time.Time
}

func NewClient(config ClientConfig) (*Client, error) {
	if config.HTTP == nil || !validDirectory(config.Directory) ||
		config.Dialect != DialectOpenCode && config.Dialect != DialectKilo {
		return nil, productruntime.ErrProtocol
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Client{http: config.HTTP, directory: config.Directory, dialect: config.Dialect, now: config.Now}, nil
}

func validDirectory(value string) bool {
	return strings.TrimSpace(value) == value && value != "" && utf8.ValidString(value) && filepath.IsAbs(value) && filepath.Clean(value) == value &&
		len([]byte(value)) <= 4096 && !strings.ContainsAny(value, "\x00\r\n")
}

func validNativeID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) <= len(prefix) || len(value) > maxNativeIDBytes {
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

func nativeMessageID(operationID string) (string, error) {
	if strings.TrimSpace(operationID) == "" || len(operationID) > 1024 || !utf8.ValidString(operationID) {
		return "", productruntime.ErrProtocol
	}
	digest := sha256.Sum256([]byte(operationID))
	return "msg_" + hex.EncodeToString(digest[:16]), nil
}

func (client *Client) path(route string) string {
	query := url.Values{"directory": []string{client.directory}}
	return route + "?" + query.Encode()
}

func (client *Client) request(ctx context.Context, method, route string, body any, expected ...int) (productserver.Response, error) {
	return client.requestAt(ctx, true, method, route, body, expected...)
}

// requestUnscoped is reserved for Kilo's /api/session/* v2 surface. Unlike the
// OpenCode-compatible API, those operations reject a directory query.
func (client *Client) requestUnscoped(ctx context.Context, method, route string, body any, expected ...int) (productserver.Response, error) {
	return client.requestAt(ctx, false, method, route, body, expected...)
}

func (client *Client) requestAt(ctx context.Context, scoped bool, method, route string, body any, expected ...int) (productserver.Response, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return productserver.Response{}, fmt.Errorf("%w: encode native request", productruntime.ErrProtocol)
		}
	}
	header := make(http.Header)
	if body != nil {
		header.Set("Content-Type", "application/json")
	}
	target := route
	if scoped {
		target = client.path(route)
	}
	response, err := client.http.Do(ctx, productserver.Request{Method: method, Path: target, Header: header, Body: encoded})
	if err != nil {
		return productserver.Response{}, mapTransportError(err)
	}
	for _, status := range expected {
		if response.StatusCode == status {
			return response, nil
		}
	}
	return productserver.Response{}, mapStatus(response.StatusCode)
}

func mapTransportError(err error) error {
	switch {
	case errors.Is(err, productruntime.ErrUnavailable), errors.Is(err, productruntime.ErrIncompatible),
		errors.Is(err, productruntime.ErrUnauthorized), errors.Is(err, productruntime.ErrStale),
		errors.Is(err, productruntime.ErrAmbiguousSession), errors.Is(err, productruntime.ErrUnsupportedPolicy),
		errors.Is(err, productruntime.ErrUnsupportedSteer),
		errors.Is(err, productruntime.ErrNativeRejected), errors.Is(err, productruntime.ErrProtocol),
		errors.Is(err, productruntime.ErrTimedOut):
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: native request deadline", productruntime.ErrTimedOut)
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, productserver.ErrResponseTooLarge), errors.Is(err, productserver.ErrRequestTooLarge),
		errors.Is(err, productserver.ErrInvalidResponse), errors.Is(err, productserver.ErrInvalidEventStream),
		errors.Is(err, productserver.ErrEventTooLarge):
		return fmt.Errorf("%w: bounded native response rejected", productruntime.ErrProtocol)
	default:
		return fmt.Errorf("%w: native server unavailable", productruntime.ErrUnavailable)
	}
}

func mapStatus(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return productruntime.ErrUnauthorized
	case http.StatusNotFound, http.StatusGone:
		return productruntime.ErrStale
	case http.StatusConflict, http.StatusLocked:
		return productruntime.ErrNativeRejected
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return productruntime.ErrTimedOut
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return productruntime.ErrNativeRejected
	default:
		if status >= 500 {
			return productruntime.ErrUnavailable
		}
		return productruntime.ErrProtocol
	}
}

type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	ProjectID string `json:"projectID"`
}

type PermissionRule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     string `json:"action"`
}

type NativeModel struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

func (client *Client) CreateSession(ctx context.Context, title string, permissions []PermissionRule) (Session, error) {
	if len([]byte(title)) > maxTitleBytes || !utf8.ValidString(title) || strings.ContainsRune(title, '\x00') {
		return Session{}, productruntime.ErrProtocol
	}
	for _, rule := range permissions {
		if rule.Permission == "" || rule.Pattern == "" || len(rule.Permission) > maxNativeIDBytes || len(rule.Pattern) > maxTitleBytes ||
			strings.ContainsAny(rule.Permission, "\x00\r\n") || strings.ContainsAny(rule.Pattern, "\x00\r\n") ||
			rule.Action != "ask" && rule.Action != "allow" && rule.Action != "deny" {
			return Session{}, productruntime.ErrUnsupportedPolicy
		}
	}
	body := struct {
		Title      string           `json:"title,omitempty"`
		Permission []PermissionRule `json:"permission,omitempty"`
	}{Title: title, Permission: permissions}
	response, err := client.request(ctx, http.MethodPost, "/session", body, http.StatusOK)
	if err != nil {
		return Session{}, err
	}
	var session Session
	if json.Unmarshal(response.Body, &session) != nil || !validNativeID(session.ID, "ses_") {
		return Session{}, productruntime.ErrProtocol
	}
	return session, nil
}

func (client *Client) GetSession(ctx context.Context, nativeSessionID string) (Session, error) {
	if !validNativeID(nativeSessionID, "ses_") {
		return Session{}, productruntime.ErrProtocol
	}
	response, err := client.request(ctx, http.MethodGet, "/session/"+url.PathEscape(nativeSessionID), nil, http.StatusOK)
	if err != nil {
		return Session{}, err
	}
	var session Session
	if json.Unmarshal(response.Body, &session) != nil || session.ID != nativeSessionID {
		return Session{}, productruntime.ErrAmbiguousSession
	}
	return session, nil
}

// SelectKiloSession uses Kilo's documented full-TUI controller to navigate an
// attached client to one exact native session. It never substitutes the last
// or most-recent session when the requested ID is gone.
func (client *Client) SelectKiloSession(ctx context.Context, nativeSessionID string) error {
	if client.dialect != DialectKilo || !validNativeID(nativeSessionID, "ses_") {
		return productruntime.ErrProtocol
	}
	response, err := client.request(ctx, http.MethodPost, "/tui/select-session",
		map[string]string{"sessionID": nativeSessionID}, http.StatusOK)
	if err != nil {
		if errors.Is(err, productruntime.ErrStale) {
			return fmt.Errorf("%w: exact Kilo native session is gone", productruntime.ErrStale)
		}
		return err
	}
	var selected bool
	if json.Unmarshal(response.Body, &selected) != nil || !selected {
		return productruntime.ErrNativeRejected
	}
	return nil
}

func (client *Client) RenameSession(ctx context.Context, nativeSessionID, title string) error {
	if !validNativeID(nativeSessionID, "ses_") || strings.TrimSpace(title) == "" ||
		len([]byte(title)) > maxTitleBytes || !utf8.ValidString(title) || strings.ContainsRune(title, '\x00') {
		return productruntime.ErrProtocol
	}
	response, err := client.request(ctx, http.MethodPatch, "/session/"+url.PathEscape(nativeSessionID),
		map[string]string{"title": title}, http.StatusOK)
	if err != nil {
		return err
	}
	var session Session
	if json.Unmarshal(response.Body, &session) != nil || session.ID != nativeSessionID || session.Title != title {
		return productruntime.ErrAmbiguousSession
	}
	return nil
}

// PromptAsync uses the stable documented OpenCode session API. The caller
// supplies a deterministic native message ID, making a 204 exact acceptance.
func (client *Client) PromptAsync(ctx context.Context, nativeSessionID, operationID string, content []byte, noReply bool, model *NativeModel) (productruntime.NativeAcceptance, error) {
	if !validNativeID(nativeSessionID, "ses_") || !utf8.Valid(content) || len(content) == 0 || len(content) > maxResultBytes {
		return productruntime.NativeAcceptance{}, productruntime.ErrProtocol
	}
	if model != nil && (!validModelPart(model.ProviderID) || !validModelPart(model.ModelID)) {
		return productruntime.NativeAcceptance{}, productruntime.ErrProtocol
	}
	messageID, err := nativeMessageID(operationID)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	body := map[string]any{
		"messageID": messageID,
		"noReply":   noReply,
		"parts":     []map[string]string{{"type": "text", "text": string(content)}},
	}
	if model != nil {
		body["model"] = model
	}
	_, err = client.request(ctx, http.MethodPost, "/session/"+url.PathEscape(nativeSessionID)+"/prompt_async", body, http.StatusNoContent)
	if err != nil {
		return productruntime.NativeAcceptance{}, err
	}
	return productruntime.NativeAcceptance{NativeSessionID: nativeSessionID, NativeMessageID: messageID, AcceptedAt: client.now().UTC()}, nil
}

func validModelPart(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= maxNativeIDBytes &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func (client *Client) Interrupt(ctx context.Context, nativeSessionID string) error {
	if !validNativeID(nativeSessionID, "ses_") {
		return productruntime.ErrProtocol
	}
	if client.dialect == DialectKilo {
		_, err := client.requestUnscoped(ctx, http.MethodPost, "/api/session/"+url.PathEscape(nativeSessionID)+"/interrupt", struct{}{}, http.StatusNoContent)
		return err
	}
	response, err := client.request(ctx, http.MethodPost, "/session/"+url.PathEscape(nativeSessionID)+"/abort", struct{}{}, http.StatusOK)
	if err != nil {
		return err
	}
	var accepted bool
	if json.Unmarshal(response.Body, &accepted) != nil || !accepted {
		return productruntime.ErrNativeRejected
	}
	return nil
}

func (client *Client) DeleteSession(ctx context.Context, nativeSessionID string) error {
	if !validNativeID(nativeSessionID, "ses_") {
		return productruntime.ErrProtocol
	}
	response, err := client.request(ctx, http.MethodDelete, "/session/"+url.PathEscape(nativeSessionID), nil, http.StatusOK, http.StatusNotFound)
	if errors.Is(err, productruntime.ErrStale) {
		return nil
	}
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusNotFound {
		return nil
	}
	var deleted bool
	if json.Unmarshal(response.Body, &deleted) != nil || !deleted {
		return productruntime.ErrNativeRejected
	}
	return nil
}

type message struct {
	Info struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
		Role      string `json:"role"`
		Finish    string `json:"finish"`
		Time      struct {
			Completed *int64 `json:"completed"`
		} `json:"time"`
		Error json.RawMessage `json:"error"`
	} `json:"info"`
	Parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts"`
}

func terminalAssistantFinish(dialect Dialect, finish string) bool {
	if finish == "" || finish == "tool-calls" {
		return false
	}
	return dialect == DialectKilo || finish != "unknown"
}

func (client *Client) messages(ctx context.Context, nativeSessionID string) ([]message, error) {
	response, err := client.request(ctx, http.MethodGet, "/session/"+url.PathEscape(nativeSessionID)+"/message", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var messages []message
	if json.Unmarshal(response.Body, &messages) != nil || len(messages) > 4096 {
		return nil, productruntime.ErrProtocol
	}
	for _, candidate := range messages {
		if candidate.Info.SessionID != "" && candidate.Info.SessionID != nativeSessionID {
			return nil, productruntime.ErrAmbiguousSession
		}
	}
	return messages, nil
}

// TurnCompleted uses the exact admitted user message as a lower bound, then
// follows the product's own ordered transcript to its first completed assistant.
// Product-owned synthetic or intermediate messages may sit between the two.
func (client *Client) TurnCompleted(ctx context.Context, nativeSessionID, nativeMessageID string) (bool, error) {
	if !validNativeID(nativeSessionID, "ses_") || !validNativeID(nativeMessageID, "msg_") {
		return false, productruntime.ErrProtocol
	}
	messages, err := client.messages(ctx, nativeSessionID)
	if err != nil {
		return false, err
	}
	foundInput := false
	for _, candidate := range messages {
		if candidate.Info.ID == nativeMessageID {
			if candidate.Info.Role != "user" {
				return false, productruntime.ErrAmbiguousSession
			}
			foundInput = true
			continue
		}
		if !foundInput || candidate.Info.Role != "assistant" || candidate.Info.Time.Completed == nil {
			continue
		}
		if *candidate.Info.Time.Completed <= 0 {
			return false, productruntime.ErrProtocol
		}
		if len(bytes.TrimSpace(candidate.Info.Error)) != 0 && !bytes.Equal(bytes.TrimSpace(candidate.Info.Error), []byte("null")) {
			return false, productruntime.ErrNativeRejected
		}
		if !terminalAssistantFinish(client.dialect, candidate.Info.Finish) {
			continue
		}
		return true, nil
	}
	return false, nil
}

func (client *Client) ResultAfter(ctx context.Context, nativeSessionID, nativeMessageID string) (string, error) {
	if !validNativeID(nativeSessionID, "ses_") || !validNativeID(nativeMessageID, "msg_") {
		return "", productruntime.ErrProtocol
	}
	messages, err := client.messages(ctx, nativeSessionID)
	if err != nil {
		return "", err
	}
	found := false
	var result bytes.Buffer
	for _, candidate := range messages {
		if candidate.Info.ID == nativeMessageID {
			if candidate.Info.Role != "user" {
				return "", productruntime.ErrAmbiguousSession
			}
			found = true
			continue
		}
		if !found || candidate.Info.Role != "assistant" || candidate.Info.Time.Completed == nil {
			continue
		}
		if *candidate.Info.Time.Completed <= 0 {
			return "", productruntime.ErrProtocol
		}
		if len(bytes.TrimSpace(candidate.Info.Error)) != 0 && !bytes.Equal(bytes.TrimSpace(candidate.Info.Error), []byte("null")) {
			return "", productruntime.ErrNativeRejected
		}
		if !terminalAssistantFinish(client.dialect, candidate.Info.Finish) {
			continue
		}
		for _, part := range candidate.Parts {
			if part.Type != "text" || part.Text == "" {
				continue
			}
			if result.Len() > 0 {
				result.WriteByte('\n')
			}
			if result.Len()+len(part.Text) > maxResultBytes {
				return "", productruntime.ErrProtocol
			}
			result.WriteString(part.Text)
		}
		return result.String(), nil
	}
	return "", productruntime.ErrAmbiguousSession
}

type NativeEvent struct {
	Type         string
	SessionID    string
	PermissionID string
	State        string
	Detail       string
}

func decodeEvent(event productserver.Event) (NativeEvent, error) {
	var envelope struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionID"`
		Data      struct {
			ID        string          `json:"id"`
			SessionID string          `json:"sessionID"`
			Error     json.RawMessage `json:"error"`
		} `json:"data"`
		Properties struct {
			SessionID  string `json:"sessionID"`
			Permission struct {
				ID        string `json:"id"`
				SessionID string `json:"sessionID"`
			} `json:"permission"`
			ID     string `json:"id"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
			Reason string          `json:"reason"`
			Error  json.RawMessage `json:"error"`
		} `json:"properties"`
	}
	if json.Unmarshal([]byte(event.Data), &envelope) != nil {
		return NativeEvent{}, productruntime.ErrProtocol
	}
	result := NativeEvent{Type: envelope.Type, SessionID: envelope.SessionID}
	if result.Type == "" {
		result.Type = event.Type
	}
	if result.SessionID == "" {
		result.SessionID = envelope.Properties.SessionID
	}
	if result.SessionID == "" {
		result.SessionID = envelope.Data.SessionID
	}
	result.PermissionID = envelope.Properties.Permission.ID
	if result.PermissionID == "" {
		result.PermissionID = envelope.Properties.ID
	}
	if result.PermissionID == "" {
		result.PermissionID = envelope.Data.ID
	}
	if result.SessionID == "" {
		result.SessionID = envelope.Properties.Permission.SessionID
	}
	result.State = envelope.Properties.Status.Type
	if result.State == "" {
		result.State = envelope.Properties.Reason
	}
	result.Detail = nativeEventError(envelope.Properties.Error, envelope.Data.Error)
	if result.Type == "" || len(result.Type) > maxNativeIDBytes {
		return NativeEvent{}, productruntime.ErrProtocol
	}
	return result, nil
}

func nativeEventError(values ...json.RawMessage) string {
	for _, raw := range values {
		var payload struct {
			Message string `json:"message"`
			Data    struct {
				Message string `json:"message"`
			} `json:"data"`
		}
		if len(raw) == 0 || json.Unmarshal(raw, &payload) != nil {
			continue
		}
		if payload.Data.Message != "" {
			return payload.Data.Message
		}
		if payload.Message != "" {
			return payload.Message
		}
	}
	return ""
}

type PermissionDecision func(context.Context, NativeEvent) (string, error)

var errTurnTerminal = errors.New("opencode-family turn terminal")

func (client *Client) WaitIdle(ctx context.Context, nativeSessionID string, decide PermissionDecision) error {
	if !validNativeID(nativeSessionID, "ses_") {
		return productruntime.ErrProtocol
	}
	streamPath := client.path("/event")
	if client.dialect == DialectKilo {
		streamPath = "/api/session/" + url.PathEscape(nativeSessionID) + "/event"
	}
	var productError string
	err := client.http.Subscribe(ctx, productserver.EventOptions{Path: streamPath}, func(event productserver.Event) error {
		native, err := decodeEvent(event)
		if err != nil {
			return err
		}
		if native.SessionID != "" && native.SessionID != nativeSessionID {
			return nil
		}
		switch native.Type {
		case "permission.asked", "permission.v2.asked":
			if decide == nil || !validNativeID(native.PermissionID, "per_") {
				return productruntime.ErrNativeRejected
			}
			decision, err := decide(ctx, native)
			if err != nil {
				return err
			}
			return client.ReplyPermission(ctx, nativeSessionID, native.PermissionID, decision)
		case "session.error":
			if native.Detail != "" {
				productError = native.Detail
				return errTurnTerminal
			}
			return productruntime.ErrNativeRejected
		case "session.idle":
			return errTurnTerminal
		case "session.turn.close":
			switch native.State {
			case "completed":
				return errTurnTerminal
			case "error", "interrupted", "superseded":
				return productruntime.ErrNativeRejected
			default:
				return productruntime.ErrProtocol
			}
		case "session.status":
			if native.State == "idle" {
				return errTurnTerminal
			}
		}
		return nil
	})
	if productError != "" {
		return errors.New(productError)
	}
	if errors.Is(err, errTurnTerminal) {
		return nil
	}
	return mapTransportError(err)
}

const turnReconcileInterval = 25 * time.Millisecond

// WaitTurn races the bounded event stream with exact native message
// reconciliation. An SSE terminal event is advisory until the admitted user
// message has a completed assistant child, so a terminal emitted before
// subscription cannot be lost or confused with another turn.
func (client *Client) WaitTurn(ctx context.Context, nativeSessionID, nativeMessageID string, decide PermissionDecision) error {
	if ctx == nil || !validNativeID(nativeSessionID, "ses_") || !validNativeID(nativeMessageID, "msg_") {
		return productruntime.ErrProtocol
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	streamResult := make(chan error, 1)
	go func() { streamResult <- client.WaitIdle(streamCtx, nativeSessionID, decide) }()
	streamLive := true
	defer func() {
		cancelStream()
		if streamLive {
			<-streamResult
		}
	}()

	ticker := time.NewTicker(turnReconcileInterval)
	defer ticker.Stop()
	for {
		completed, err := client.TurnCompleted(ctx, nativeSessionID, nativeMessageID)
		if err != nil {
			return err
		}
		if completed {
			return nil
		}
		select {
		case err := <-streamResult:
			streamLive = false
			streamResult = nil
			if err != nil {
				return err
			}
			// A terminal event may precede persistence of the completed assistant
			// message. Continue bounded reconciliation under the caller deadline.
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (client *Client) ReplyPermission(ctx context.Context, sessionID, permissionID, decision string) error {
	if !validNativeID(sessionID, "ses_") || !validNativeID(permissionID, "per_") ||
		(decision != "once" && decision != "always" && decision != "reject") {
		return productruntime.ErrProtocol
	}
	if client.dialect == DialectKilo {
		_, err := client.requestUnscoped(ctx, http.MethodPost,
			"/api/session/"+url.PathEscape(sessionID)+"/permission/"+url.PathEscape(permissionID)+"/reply",
			map[string]string{"reply": decision}, http.StatusNoContent)
		return err
	}
	_, err := client.request(ctx, http.MethodPost,
		"/session/"+url.PathEscape(sessionID)+"/permissions/"+url.PathEscape(permissionID),
		map[string]string{"response": decision}, http.StatusOK)
	return err
}

func (client *Client) ProbeDocument(ctx context.Context, required []string) (map[string]bool, error) {
	response, err := client.requestUnscoped(ctx, http.MethodGet, "/doc", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var document struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if json.Unmarshal(response.Body, &document) != nil || document.Paths == nil {
		return nil, productruntime.ErrProtocol
	}
	features := make(map[string]bool, len(required))
	for _, route := range required {
		if route == "" || !strings.HasPrefix(route, "/") || len(route) > maxNativeIDBytes {
			return nil, productruntime.ErrProtocol
		}
		_, features[route] = document.Paths[route]
	}
	return features, nil
}

func (client *Client) ProvidersAvailable(ctx context.Context) (bool, error) {
	response, err := client.request(ctx, http.MethodGet, "/config/providers", nil, http.StatusOK)
	if err != nil {
		return false, err
	}
	var configured struct {
		Providers []json.RawMessage `json:"providers"`
		Default   map[string]string `json:"default"`
	}
	if json.Unmarshal(response.Body, &configured) != nil || len(configured.Providers) > 4096 {
		return false, productruntime.ErrProtocol
	}
	for _, provider := range configured.Providers {
		trimmed := bytes.TrimSpace(provider)
		if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
			return false, productruntime.ErrProtocol
		}
	}
	return len(configured.Providers) > 0, nil
}
