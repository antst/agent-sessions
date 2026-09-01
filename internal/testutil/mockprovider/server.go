// Package mockprovider supplies a deterministic, bounded OpenAI-compatible
// model fixture for native-product integration tests. It implements model
// endpoints only; it deliberately does not emulate any native product, session,
// transport, or tool runtime.
package mockprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Redacted replaces recognized credential values in captured requests.
const Redacted = "[REDACTED]"

// ErrClosed is returned by wait operations after the fixture closes.
var ErrClosed = errors.New("mockprovider: server closed")

var (
	errCaptureLimit    = errors.New("capture limit reached")
	errScriptExhausted = errors.New("script exhausted")
)

// CapturedRequest is a sanitized, immutable snapshot of an accepted completion
// request. Requests and WaitRequest return fresh deep copies.
type CapturedRequest struct {
	Sequence uint64
	Turn     string
	Method   string
	Path     string
	Header   http.Header
	Query    url.Values
	Model    string
	Stream   bool
	Body     map[string]any
}

// Cancellation records a request canceled while its scripted turn was blocked
// or while its response was being written. Cause never includes request data.
type Cancellation struct {
	Sequence uint64
	Turn     string
	Cause    string
}

// Server is an in-memory OpenAI-compatible HTTP model provider.
type Server struct {
	httpServer *httptest.Server
	script     Script
	limits     Limits

	mu                sync.Mutex
	closed            bool
	nextTurn          int
	nextSequence      uint64
	requests          []CapturedRequest
	cancellations     []Cancellation
	cancellationBySeq map[uint64]bool
	pending           map[uint64]chan struct{}
	requestReady      chan CapturedRequest
	cancellationReady chan Cancellation
	done              chan struct{}
	closeOnce         sync.Once
}

// New validates and starts a model fixture. Call Close when finished.
func New(script Script, options ...Option) (*Server, error) {
	configuration := defaultConfig()
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("mockprovider: nil option")
		}
		if err := option(&configuration); err != nil {
			return nil, err
		}
	}
	if configuration.limits.MaxRequestBytes <= 0 ||
		configuration.limits.MaxCapturedRequests <= 0 ||
		configuration.limits.MaxMessages <= 0 ||
		configuration.limits.MaxTools <= 0 {
		return nil, fmt.Errorf("mockprovider: all limits must be positive")
	}
	prepared, err := prepareScript(script)
	if err != nil {
		return nil, err
	}
	server := &Server{
		script:            prepared,
		limits:            configuration.limits,
		requests:          make([]CapturedRequest, 0, min(len(prepared.Turns), configuration.limits.MaxCapturedRequests)),
		cancellations:     make([]Cancellation, 0),
		cancellationBySeq: make(map[uint64]bool),
		pending:           make(map[uint64]chan struct{}),
		requestReady:      make(chan CapturedRequest, configuration.limits.MaxCapturedRequests),
		cancellationReady: make(chan Cancellation, configuration.limits.MaxCapturedRequests),
		done:              make(chan struct{}),
	}
	httpServer := httptest.NewUnstartedServer(server)
	httpServer.Config.MaxHeaderBytes = int(configuration.limits.MaxRequestBytes)
	httpServer.Start()
	server.httpServer = httpServer
	return server, nil
}

// Start is the testing.TB convenience for New. It fails the test on invalid
// configuration and registers Close as cleanup.
func Start(t testing.TB, script Script, options ...Option) *Server {
	t.Helper()
	server, err := New(script, options...)
	if err != nil {
		t.Fatalf("start mock model provider: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close mock model provider: %v", err)
		}
	})
	return server
}

// BaseURL returns the versioned OpenAI base URL.
func (s *Server) BaseURL() string {
	return s.httpServer.URL + "/v1"
}

// Client returns the local HTTP client configured by httptest.
func (s *Server) Client() *http.Client {
	return s.httpServer.Client()
}

// Close releases blocked turns, closes active client connections, and stops
// the fixture. It is safe to call more than once.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.done)
		for sequence, release := range s.pending {
			delete(s.pending, sequence)
			close(release)
		}
		s.mu.Unlock()
		s.httpServer.CloseClientConnections()
		s.httpServer.Close()
	})
	return nil
}

// Requests returns accepted requests in deterministic capture-sequence order.
func (s *Server) Requests() []CapturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	requests := make([]CapturedRequest, len(s.requests))
	for index := range s.requests {
		requests[index] = cloneCapturedRequest(s.requests[index])
	}
	return requests
}

// WaitRequest waits for the next accepted request not already returned by this
// method. The same request remains available in Requests.
func (s *Server) WaitRequest(ctx context.Context) (CapturedRequest, error) {
	select {
	case request := <-s.requestReady:
		return cloneCapturedRequest(request), nil
	case <-ctx.Done():
		return CapturedRequest{}, ctx.Err()
	case <-s.done:
		return CapturedRequest{}, ErrClosed
	}
}

// Cancellations returns all observed cancellations in observation order.
func (s *Server) Cancellations() []Cancellation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Cancellation(nil), s.cancellations...)
}

// WaitCancellation waits for the next cancellation not already returned by
// this method. The same cancellation remains available in Cancellations.
func (s *Server) WaitCancellation(ctx context.Context) (Cancellation, error) {
	select {
	case cancellation := <-s.cancellationReady:
		return cancellation, nil
	case <-ctx.Done():
		return Cancellation{}, ctx.Err()
	case <-s.done:
		return Cancellation{}, ErrClosed
	}
}

// Release allows one blocked request to continue. Sequence comes from a
// CapturedRequest. It returns false for unknown, completed, canceled, or already
// released requests.
func (s *Server) Release(sequence uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, ok := s.pending[sequence]
	if !ok {
		return false
	}
	delete(s.pending, sequence)
	close(release)
	return true
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/v1/models":
		s.serveModels(writer)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/chat/completions":
		s.serveCompletion(writer, request)
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "not_found_error", "model endpoint not found")
	}
}

func (s *Server) serveModels(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"object": "list",
		"data": []any{map[string]any{
			"id":       s.script.Model,
			"object":   "model",
			"created":  s.script.Created,
			"owned_by": "agent-sessions",
		}},
	})
}

func (s *Server) serveCompletion(writer http.ResponseWriter, request *http.Request) {
	body, stream, model, status, decodeErr := s.decodeRequest(writer, request)
	if decodeErr != nil {
		writeAPIError(writer, status, "invalid_request", "invalid_request_error", decodeErr.Error())
		return
	}

	captured := CapturedRequest{
		Method: request.Method,
		Path:   request.URL.Path,
		Header: redactHeader(request.Header),
		Query:  redactQuery(request.URL.Query()),
		Model:  model,
		Stream: stream,
		Body:   redactMap(body),
	}
	captured, turn, release, reserveErr := s.reserve(captured)
	if reserveErr != nil {
		switch {
		case errors.Is(reserveErr, errCaptureLimit):
			writeAPIError(writer, http.StatusTooManyRequests, "capture_limit", "mock_fixture_error", "mock provider capture limit reached")
		case errors.Is(reserveErr, errScriptExhausted):
			writeAPIError(writer, http.StatusServiceUnavailable, "script_exhausted", "mock_fixture_error", "mock provider script exhausted")
		default:
			writeAPIError(writer, http.StatusServiceUnavailable, "server_closed", "mock_fixture_error", "mock provider closed")
		}
		return
	}
	if release != nil && !s.waitForRelease(request.Context(), captured, release) {
		return
	}

	if turn.Disconnect {
		s.disconnect(writer)
		return
	}
	if turn.Error != nil {
		writeAPIError(writer, turn.Error.Status, turn.Error.Code, turn.Error.Type, turn.Error.Message)
		return
	}
	if turn.MalformedStream != nil {
		s.writeMalformedStream(writer, request, captured, turn)
		return
	}
	if captured.Stream {
		s.writeStreamingCompletion(writer, request, captured, turn)
		return
	}
	s.writeJSONCompletion(writer, captured, turn)
}

func (s *Server) decodeRequest(writer http.ResponseWriter, request *http.Request) (map[string]any, bool, string, int, error) {
	if request.ContentLength > s.limits.MaxRequestBytes {
		return nil, false, "", http.StatusRequestEntityTooLarge, errors.New("request body exceeds mock provider limit")
	}
	limited := http.MaxBytesReader(writer, request.Body, s.limits.MaxRequestBytes)
	decoder := json.NewDecoder(limited)
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, false, "", http.StatusRequestEntityTooLarge, errors.New("request body exceeds mock provider limit")
		}
		return nil, false, "", http.StatusBadRequest, errors.New("request body is not valid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, false, "", http.StatusBadRequest, errors.New("request body contains multiple JSON values")
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, false, "", http.StatusRequestEntityTooLarge, errors.New("request body exceeds mock provider limit")
		}
		return nil, false, "", http.StatusBadRequest, errors.New("request body has trailing invalid JSON")
	}
	body, ok := value.(map[string]any)
	if !ok {
		return nil, false, "", http.StatusBadRequest, errors.New("request body must be a JSON object")
	}
	if err := validateCollection(body, "messages", s.limits.MaxMessages); err != nil {
		return nil, false, "", http.StatusBadRequest, err
	}
	if err := validateCollection(body, "tools", s.limits.MaxTools); err != nil {
		return nil, false, "", http.StatusBadRequest, err
	}
	stream := false
	if value, exists := body["stream"]; exists {
		var valid bool
		stream, valid = value.(bool)
		if !valid {
			return nil, false, "", http.StatusBadRequest, errors.New("stream must be a boolean")
		}
	}
	model := ""
	if value, exists := body["model"]; exists {
		var valid bool
		model, valid = value.(string)
		if !valid {
			return nil, false, "", http.StatusBadRequest, errors.New("model must be a string")
		}
	}
	return body, stream, model, 0, nil
}

func validateCollection(body map[string]any, field string, maximum int) error {
	value, exists := body[field]
	if !exists {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s must be an array", field)
	}
	if len(items) > maximum {
		return fmt.Errorf("%s exceeds mock provider limit of %d", field, maximum)
	}
	return nil
}

func (s *Server) reserve(captured CapturedRequest) (CapturedRequest, Turn, chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return CapturedRequest{}, Turn{}, nil, ErrClosed
	}
	if len(s.requests) >= s.limits.MaxCapturedRequests {
		return CapturedRequest{}, Turn{}, nil, errCaptureLimit
	}
	if s.nextTurn >= len(s.script.Turns) {
		return CapturedRequest{}, Turn{}, nil, errScriptExhausted
	}
	s.nextSequence++
	captured.Sequence = s.nextSequence
	turn := s.script.Turns[s.nextTurn]
	s.nextTurn++
	captured.Turn = turn.Name
	var release chan struct{}
	if turn.Block {
		release = make(chan struct{})
		s.pending[captured.Sequence] = release
	}
	retained := cloneCapturedRequest(captured)
	s.requests = append(s.requests, retained)
	s.requestReady <- cloneCapturedRequest(retained)
	return captured, turn, release, nil
}

func (s *Server) waitForRelease(ctx context.Context, captured CapturedRequest, release chan struct{}) bool {
	select {
	case <-release:
		return true
	case <-ctx.Done():
		s.removePending(captured.Sequence)
		s.recordCancellation(captured, ctx.Err())
		return false
	case <-s.done:
		s.removePending(captured.Sequence)
		return false
	}
}

func (s *Server) removePending(sequence uint64) {
	s.mu.Lock()
	delete(s.pending, sequence)
	s.mu.Unlock()
}

func (s *Server) recordCancellation(captured CapturedRequest, cause error) {
	if cause == nil {
		return
	}
	cancellation := Cancellation{Sequence: captured.Sequence, Turn: captured.Turn, Cause: cause.Error()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancellationBySeq[captured.Sequence] {
		return
	}
	s.cancellationBySeq[captured.Sequence] = true
	s.cancellations = append(s.cancellations, cancellation)
	s.cancellationReady <- cancellation
}

func (s *Server) disconnect(writer http.ResponseWriter) {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		writeAPIError(writer, http.StatusInternalServerError, "disconnect_unavailable", "mock_fixture_error", "connection hijacking unavailable")
		return
	}
	connection, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	_ = connection.Close()
}

func (s *Server) writeMalformedStream(writer http.ResponseWriter, request *http.Request, captured CapturedRequest, turn Turn) {
	prepareSSE(writer)
	flusher, _ := writer.(http.Flusher)
	for _, frame := range turn.MalformedStream {
		if _, err := fmt.Fprintf(writer, "data: %s\n\n", frame); err != nil {
			s.recordCancellation(captured, request.Context().Err())
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (s *Server) writeStreamingCompletion(writer http.ResponseWriter, request *http.Request, captured CapturedRequest, turn Turn) {
	prepareSSE(writer)
	flusher, _ := writer.(http.Flusher)
	wroteOutput := false
	for index, text := range turn.Text {
		delta := map[string]any{"content": text}
		if index == 0 {
			delta["role"] = "assistant"
		}
		if err := s.writeChunk(writer, flusher, captured, delta, nil, false); err != nil {
			s.recordCancellation(captured, request.Context().Err())
			return
		}
		wroteOutput = true
	}
	for index, call := range turn.ToolCalls {
		callID := call.ID
		if callID == "" {
			callID = fmt.Sprintf("call_mock_%06d_%02d", captured.Sequence, index+1)
		}
		delta := map[string]any{
			"tool_calls": []any{map[string]any{
				"index": index,
				"id":    callID,
				"type":  "function",
				"function": map[string]any{
					"name":      call.Name,
					"arguments": call.Arguments,
				},
			}},
		}
		if !wroteOutput {
			delta["role"] = "assistant"
		}
		if err := s.writeChunk(writer, flusher, captured, delta, nil, false); err != nil {
			s.recordCancellation(captured, request.Context().Err())
			return
		}
		wroteOutput = true
	}
	if !wroteOutput {
		if err := s.writeChunk(writer, flusher, captured, map[string]any{"role": "assistant", "content": ""}, nil, false); err != nil {
			s.recordCancellation(captured, request.Context().Err())
			return
		}
	}
	finish := "stop"
	if len(turn.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	if err := s.writeChunk(writer, flusher, captured, map[string]any{}, &finish, true); err != nil {
		s.recordCancellation(captured, request.Context().Err())
		return
	}
	if _, err := io.WriteString(writer, "data: [DONE]\n\n"); err != nil {
		s.recordCancellation(captured, request.Context().Err())
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) writeChunk(writer io.Writer, flusher http.Flusher, captured CapturedRequest, delta map[string]any, finish *string, includeUsage bool) error {
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": finish,
	}
	body := map[string]any{
		"id":      completionID(captured.Sequence),
		"object":  "chat.completion.chunk",
		"created": s.script.Created,
		"model":   s.script.Model,
		"choices": []any{choice},
	}
	if includeUsage {
		body["usage"] = usage()
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "data: %s\n\n", encoded); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func (s *Server) writeJSONCompletion(writer http.ResponseWriter, captured CapturedRequest, turn Turn) {
	content := strings.Join(turn.Text, "")
	message := map[string]any{"role": "assistant", "content": content}
	finish := "stop"
	if len(turn.ToolCalls) > 0 {
		finish = "tool_calls"
		if len(turn.Text) == 0 {
			message["content"] = nil
		}
		calls := make([]any, len(turn.ToolCalls))
		for index, call := range turn.ToolCalls {
			callID := call.ID
			if callID == "" {
				callID = fmt.Sprintf("call_mock_%06d_%02d", captured.Sequence, index+1)
			}
			calls[index] = map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      call.Name,
					"arguments": call.Arguments,
				},
			}
		}
		message["tool_calls"] = calls
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"id":      completionID(captured.Sequence),
		"object":  "chat.completion",
		"created": s.script.Created,
		"model":   s.script.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": usage(),
	})
}

func prepareSSE(writer http.ResponseWriter) {
	header := writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)
}

func writeAPIError(writer http.ResponseWriter, status int, code, errorType, message string) {
	writeJSON(writer, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"type":    errorType,
			"message": message,
		},
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func completionID(sequence uint64) string {
	return fmt.Sprintf("chatcmpl-mock-%06d", sequence)
}

func usage() map[string]any {
	return map[string]any{
		"prompt_tokens":     1,
		"completion_tokens": 1,
		"total_tokens":      2,
	}
}

func redactHeader(header http.Header) http.Header {
	redacted := header.Clone()
	for name := range redacted {
		if isCredentialName(name) {
			redacted[name] = []string{Redacted}
		}
	}
	return redacted
}

func redactQuery(query url.Values) url.Values {
	redacted := make(url.Values, len(query))
	for name, values := range query {
		if isCredentialName(name) {
			redacted[name] = []string{Redacted}
			continue
		}
		redacted[name] = append([]string(nil), values...)
	}
	return redacted
}

func redactMap(value map[string]any) map[string]any {
	redacted := make(map[string]any, len(value))
	for name, item := range value {
		if isCredentialName(name) {
			redacted[name] = Redacted
			continue
		}
		redacted[name] = cloneAndRedact(item)
	}
	return redacted
}

func cloneAndRedact(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return redactMap(typed)
	case []any:
		clone := make([]any, len(typed))
		for index := range typed {
			clone[index] = cloneAndRedact(typed[index])
		}
		return clone
	default:
		return typed
	}
}

func cloneCapturedRequest(request CapturedRequest) CapturedRequest {
	request.Header = request.Header.Clone()
	request.Query = cloneQuery(request.Query)
	request.Body = cloneMap(request.Body)
	return request
}

func cloneQuery(query url.Values) url.Values {
	clone := make(url.Values, len(query))
	for name, values := range query {
		clone[name] = append([]string(nil), values...)
	}
	return clone
}

func cloneMap(value map[string]any) map[string]any {
	clone := make(map[string]any, len(value))
	for name, item := range value {
		clone[name] = cloneValue(item)
	}
	return clone
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		clone := make([]any, len(typed))
		for index := range typed {
			clone[index] = cloneValue(typed[index])
		}
		return clone
	default:
		return typed
	}
}

func isCredentialName(name string) bool {
	normalized := strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(strings.TrimSpace(name)))
	switch normalized {
	case "authorization", "proxy_authorization", "api_key", "apikey", "key", "x_api_key",
		"access_token", "refresh_token", "id_token", "auth_token", "bearer_token", "token",
		"password", "passwd", "secret", "client_secret", "private_key", "credential", "credentials", "cookie", "set_cookie":
		return true
	}
	for _, suffix := range []string{
		"_api_key", "_token", "_authorization", "_access_token", "_refresh_token", "_id_token", "_auth_token",
		"_password", "_passwd", "_secret", "_private_key", "_credential", "_credentials",
	} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}
