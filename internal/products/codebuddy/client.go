package codebuddy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/productserver"
)

const maxNativeTextBytes = 1 << 20

// APIClient is a typed client for the pinned, self-described CodeBuddy API.
// Constructors preserve the peer/lane authentication split.
type APIClient struct {
	client   *productserver.Client
	peerCSRF bool
}

func NewPeerClient(endpoint string, limits productserver.Limits) (*APIClient, error) {
	csrf, err := productserver.NewMemoryAuth(CSRFHeader, productruntime.NewSensitiveValue(CSRFValue))
	if err != nil {
		return nil, err
	}
	client, err := productserver.NewClient(productserver.ClientConfig{Endpoint: endpoint, Auth: csrf, Limits: limits})
	if err != nil {
		return nil, err
	}
	return &APIClient{client: client, peerCSRF: true}, nil
}

func NewLaneClient(endpoint string, password productruntime.SensitiveValue, limits productserver.Limits) (*APIClient, error) {
	auth, err := productserver.NewBearerAuth(password)
	if err != nil {
		return nil, err
	}
	client, err := productserver.NewClient(productserver.ClientConfig{Endpoint: endpoint, Auth: auth, Limits: limits})
	if err != nil {
		return nil, err
	}
	return &APIClient{client: client}, nil
}

func wrapOwnedClient(client *productserver.Client) *APIClient {
	return &APIClient{client: client}
}

func (client *APIClient) CloseIdleConnections() {
	if client != nil && client.client != nil {
		client.client.CloseIdleConnections()
	}
}

type LiveSession struct {
	SessionID      string
	WriterOccupied bool
}

func (client *APIClient) LiveSession(ctx context.Context) (LiveSession, error) {
	var response struct {
		Data struct {
			SessionID      *string `json:"sessionId"`
			WriterOccupied bool    `json:"writerOccupied"`
		} `json:"data"`
	}
	if err := client.json(ctx, http.MethodGet, "/api/v1/sessions/live", nil, http.StatusOK, &response); err != nil {
		return LiveSession{}, err
	}
	if response.Data.SessionID == nil || strings.TrimSpace(*response.Data.SessionID) == "" {
		return LiveSession{}, fmt.Errorf("%w: live session identity is absent", productruntime.ErrProtocol)
	}
	return LiveSession{SessionID: *response.Data.SessionID, WriterOccupied: response.Data.WriterOccupied}, nil
}

func (client *APIClient) ReplySession(ctx context.Context, sessionID, text string) error {
	if !validNativeID(sessionID) || !validText(text) {
		return productruntime.ErrProtocol
	}
	var response struct {
		Data struct {
			Delivered bool `json:"delivered"`
		} `json:"data"`
	}
	path := "/api/v1/sessions/" + url.PathEscape(sessionID) + "/reply"
	if err := client.json(ctx, http.MethodPost, path, map[string]string{"text": text}, http.StatusOK, &response); err != nil {
		return err
	}
	if !response.Data.Delivered {
		return fmt.Errorf("%w: session reply was not delivered", productruntime.ErrNativeRejected)
	}
	return nil
}

func (client *APIClient) RenameSession(ctx context.Context, sessionID, name string) error {
	name = strings.TrimSpace(name)
	if !validNativeID(sessionID) || name == "" || len(name) > 256 || !utf8.ValidString(name) {
		return productruntime.ErrProtocol
	}
	path := "/api/v1/sessions/" + url.PathEscape(sessionID) + "/rename"
	return client.json(ctx, http.MethodPost, path, map[string]string{"name": name}, http.StatusNoContent, nil)
}

type AgentJob struct {
	ID              string            `json:"id"`
	SessionID       string            `json:"sessionId"`
	State           string            `json:"state"`
	Status          string            `json:"status"`
	Name            string            `json:"name"`
	Detail          string            `json:"detail"`
	Cwd             string            `json:"cwd"`
	StartedAt       int64             `json:"startedAt"`
	UpdatedAt       int64             `json:"updatedAt"`
	FirstTerminalAt int64             `json:"firstTerminalAt"`
	Alive           bool              `json:"alive"`
	Settled         bool              `json:"settled"`
	Output          map[string]string `json:"output"`
}

func (job AgentJob) valid() bool {
	if !validNativeID(job.ID) || !validNativeID(job.SessionID) || !filepath.IsAbs(job.Cwd) || job.StartedAt <= 0 {
		return false
	}
	switch job.State {
	case "working", "blocked", "done", "failed", "stopped":
		return true
	default:
		return false
	}
}

type DispatchJobRequest struct {
	Prompt         string `json:"prompt"`
	Cwd            string `json:"cwd,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
	Name           string `json:"name,omitempty"`
}

func (client *APIClient) DispatchJob(ctx context.Context, request DispatchJobRequest) (AgentJob, error) {
	if !validText(request.Prompt) || !filepath.IsAbs(request.Cwd) || !validNativeID(request.Name) {
		return AgentJob{}, productruntime.ErrProtocol
	}
	var response struct {
		Data AgentJob `json:"data"`
	}
	if err := client.json(ctx, http.MethodPost, "/api/v1/jobs", request, http.StatusOK, &response); err != nil {
		return AgentJob{}, err
	}
	if !response.Data.valid() {
		return AgentJob{}, fmt.Errorf("%w: dispatch returned an invalid job", productruntime.ErrProtocol)
	}
	if filepath.Clean(response.Data.Cwd) != filepath.Clean(request.Cwd) {
		return AgentJob{}, fmt.Errorf("%w: dispatch changed the requested cwd", productruntime.ErrProtocol)
	}
	if response.Data.Name != request.Name {
		return AgentJob{}, fmt.Errorf("%w: dispatch changed the exact request marker", productruntime.ErrProtocol)
	}
	return response.Data, nil
}

func (client *APIClient) ListJobs(ctx context.Context, cwd string) ([]AgentJob, error) {
	if !filepath.IsAbs(cwd) {
		return nil, productruntime.ErrProtocol
	}
	query := url.Values{"all": []string{"true"}, "cwd": []string{cwd}}
	var response struct {
		Data struct {
			Jobs []AgentJob `json:"jobs"`
		} `json:"data"`
	}
	if err := client.json(ctx, http.MethodGet, "/api/v1/jobs?"+query.Encode(), nil, http.StatusOK, &response); err != nil {
		return nil, err
	}
	const maximumJobs = 4096
	if len(response.Data.Jobs) > maximumJobs {
		return nil, fmt.Errorf("%w: job-list bound exceeded", productruntime.ErrProtocol)
	}
	seen := make(map[string]struct{}, len(response.Data.Jobs))
	for _, job := range response.Data.Jobs {
		if !validNativeID(job.ID) {
			return nil, fmt.Errorf("%w: job list contains an invalid identity", productruntime.ErrProtocol)
		}
		if _, duplicate := seen[job.ID]; duplicate {
			return nil, fmt.Errorf("%w: job list contains duplicate identity", productruntime.ErrProtocol)
		}
		seen[job.ID] = struct{}{}
	}
	return response.Data.Jobs, nil
}

func (client *APIClient) GetJob(ctx context.Context, jobID, sessionID, cwd string) (AgentJob, error) {
	if !validNativeID(jobID) || !validNativeID(sessionID) || !filepath.IsAbs(cwd) {
		return AgentJob{}, productruntime.ErrProtocol
	}
	var response struct {
		Data struct {
			Job AgentJob `json:"job"`
		} `json:"data"`
	}
	if err := client.json(ctx, http.MethodGet, "/api/v1/jobs/"+url.PathEscape(jobID), nil, http.StatusOK, &response); err != nil {
		return AgentJob{}, err
	}
	if !response.Data.Job.valid() || response.Data.Job.ID != jobID || response.Data.Job.SessionID != sessionID ||
		filepath.Clean(response.Data.Job.Cwd) != filepath.Clean(cwd) {
		return AgentJob{}, fmt.Errorf("%w: job detail identity changed", productruntime.ErrProtocol)
	}
	return response.Data.Job, nil
}

type JobReply struct {
	Delivered bool
	Saved     bool
}

func (client *APIClient) ReplyJob(ctx context.Context, jobID, text string) (JobReply, error) {
	if !validNativeID(jobID) || !validText(text) {
		return JobReply{}, productruntime.ErrProtocol
	}
	var response struct {
		Data struct {
			Delivered bool   `json:"delivered"`
			Saved     bool   `json:"saved"`
			Notice    string `json:"notice"`
		} `json:"data"`
	}
	path := "/api/v1/jobs/" + url.PathEscape(jobID) + "/reply"
	if err := client.json(ctx, http.MethodPost, path, map[string]any{"text": text, "bash": false}, http.StatusOK, &response); err != nil {
		return JobReply{}, err
	}
	if response.Data.Delivered == response.Data.Saved {
		return JobReply{}, fmt.Errorf("%w: job reply has ambiguous acceptance", productruntime.ErrProtocol)
	}
	return JobReply{Delivered: response.Data.Delivered, Saved: response.Data.Saved}, nil
}

func (client *APIClient) StopJob(ctx context.Context, jobID string) error {
	if !validNativeID(jobID) {
		return productruntime.ErrProtocol
	}
	var response struct {
		Data struct {
			Stopped bool `json:"stopped"`
		} `json:"data"`
	}
	path := "/api/v1/jobs/" + url.PathEscape(jobID) + "/stop"
	if err := client.json(ctx, http.MethodPost, path, nil, http.StatusOK, &response); err != nil {
		return err
	}
	if !response.Data.Stopped {
		return fmt.Errorf("%w: job stop was not confirmed", productruntime.ErrNativeRejected)
	}
	return nil
}

func (client *APIClient) RespawnJob(ctx context.Context, jobID, sessionID, cwd string) (AgentJob, error) {
	if !validNativeID(jobID) || !validNativeID(sessionID) || !filepath.IsAbs(cwd) {
		return AgentJob{}, productruntime.ErrProtocol
	}
	var response struct {
		Data struct {
			Job AgentJob `json:"job"`
		} `json:"data"`
	}
	path := "/api/v1/jobs/" + url.PathEscape(jobID) + "/respawn"
	if err := client.json(ctx, http.MethodPost, path, nil, http.StatusOK, &response); err != nil {
		return AgentJob{}, err
	}
	if !response.Data.Job.valid() || response.Data.Job.ID != jobID || response.Data.Job.SessionID != sessionID ||
		filepath.Clean(response.Data.Job.Cwd) != filepath.Clean(cwd) {
		return AgentJob{}, fmt.Errorf("%w: respawn substituted another job", productruntime.ErrProtocol)
	}
	return response.Data.Job, nil
}

func (client *APIClient) ResumeJob(ctx context.Context, sessionID, cwd string) (AgentJob, error) {
	if !validNativeID(sessionID) || !filepath.IsAbs(cwd) {
		return AgentJob{}, productruntime.ErrProtocol
	}
	query := url.Values{"cwd": []string{cwd}}
	var response struct {
		Data AgentJob `json:"data"`
	}
	if err := client.json(ctx, http.MethodPost, "/api/v1/jobs/resume?"+query.Encode(), map[string]string{"sessionId": sessionID}, http.StatusOK, &response); err != nil {
		return AgentJob{}, err
	}
	if !response.Data.valid() || response.Data.SessionID != sessionID || filepath.Clean(response.Data.Cwd) != filepath.Clean(cwd) {
		return AgentJob{}, fmt.Errorf("%w: resume substituted another native session", productruntime.ErrProtocol)
	}
	return response.Data, nil
}

type DeleteJobResult struct {
	Deleted bool
	Reason  string
}

func (client *APIClient) DeleteJob(ctx context.Context, jobID string) (DeleteJobResult, error) {
	if !validNativeID(jobID) {
		return DeleteJobResult{}, productruntime.ErrProtocol
	}
	var response struct {
		Data struct {
			Deleted bool   `json:"deleted"`
			Reason  string `json:"reason"`
		} `json:"data"`
	}
	if err := client.json(ctx, http.MethodDelete, "/api/v1/jobs/"+url.PathEscape(jobID), nil, http.StatusOK, &response); err != nil {
		return DeleteJobResult{}, err
	}
	if !response.Data.Deleted && strings.TrimSpace(response.Data.Reason) == "" {
		return DeleteJobResult{}, fmt.Errorf("%w: guarded job deletion omitted its reason", productruntime.ErrProtocol)
	}
	return DeleteJobResult{Deleted: response.Data.Deleted, Reason: response.Data.Reason}, nil
}

func (client *APIClient) StreamJob(ctx context.Context, jobID string, handle func(productserver.Event) error) error {
	if client == nil || client.client == nil || !validNativeID(jobID) || handle == nil {
		return productruntime.ErrProtocol
	}
	options := productserver.EventOptions{
		Path:              "/api/v1/jobs/" + url.PathEscape(jobID) + "/stream",
		Header:            client.csrfHeader(),
		MaxLineBytes:      maxNativeTextBytes,
		MaxEventBytes:     maxNativeTextBytes,
		MaxReconnects:     3,
		ReconnectDelay:    50 * time.Millisecond,
		MaxReconnectDelay: time.Second,
		DedupWindow:       256,
	}
	if err := client.client.Subscribe(ctx, options, handle); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: stream job: %v", productruntime.ErrProtocol, err)
	}
	return nil
}

type OpenAPIDocument struct {
	Info struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]map[string]OpenAPIOperation `json:"paths"`
	Components struct {
		Schemas map[string]OpenAPISchema `json:"schemas"`
	} `json:"components"`
}

type OpenAPIOperation struct {
	OperationID string `json:"operationId"`
	RequestBody struct {
		Content map[string]struct {
			Schema OpenAPISchema `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
}

type OpenAPISchema struct {
	Type       string                   `json:"type"`
	Ref        string                   `json:"$ref"`
	Required   []string                 `json:"required"`
	Properties map[string]OpenAPISchema `json:"properties"`
	Items      *OpenAPISchema           `json:"items"`
}

func (client *APIClient) OpenAPI(ctx context.Context) (OpenAPIDocument, error) {
	var document OpenAPIDocument
	if err := client.json(ctx, http.MethodGet, "/api/openapi.json", nil, http.StatusOK, &document); err != nil {
		return OpenAPIDocument{}, err
	}
	return document, nil
}

func (client *APIClient) Health(ctx context.Context) error {
	var response struct {
		Data struct {
			Status string `json:"status"`
			PID    int    `json:"pid"`
		} `json:"data"`
	}
	if err := client.json(ctx, http.MethodGet, "/api/v1/health", nil, http.StatusOK, &response); err != nil {
		return err
	}
	if response.Data.Status != "ok" || response.Data.PID <= 1 {
		return fmt.Errorf("%w: health response is invalid", productruntime.ErrProtocol)
	}
	return nil
}

func (client *APIClient) json(ctx context.Context, method, path string, body any, expectedStatus int, output any) error {
	if client == nil || client.client == nil || ctx == nil {
		return productruntime.ErrUnavailable
	}
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%w: encode request", productruntime.ErrProtocol)
		}
	}
	header := client.csrfHeader()
	if body != nil {
		header.Set("Content-Type", "application/json")
	}
	response, err := client.client.Do(ctx, productserver.Request{Method: method, Path: path, Header: header, Body: encoded})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return errors.Join(productruntime.ErrTimedOut, err)
		}
		return fmt.Errorf("%w: native request failed", productruntime.ErrUnavailable)
	}
	if response.StatusCode != expectedStatus {
		return nativeStatusError(method+" "+pathWithoutQuery(path), response.StatusCode)
	}
	if output == nil {
		if len(strings.TrimSpace(string(response.Body))) != 0 {
			return fmt.Errorf("%w: unexpected response body", productruntime.ErrProtocol)
		}
		return nil
	}
	mediaType := response.Header.Get("Content-Type")
	if mediaType != "" && !strings.HasPrefix(strings.ToLower(mediaType), "application/json") {
		return fmt.Errorf("%w: expected JSON response", productruntime.ErrProtocol)
	}
	decoder := json.NewDecoder(strings.NewReader(string(response.Body)))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("%w: malformed JSON response", productruntime.ErrProtocol)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: malformed trailing JSON", productruntime.ErrProtocol)
	}
	return nil
}

func (client *APIClient) csrfHeader() http.Header {
	header := make(http.Header)
	if !client.peerCSRF {
		header.Set(CSRFHeader, CSRFValue)
	}
	return header
}

func pathWithoutQuery(path string) string {
	if index := strings.IndexByte(path, '?'); index >= 0 {
		return path[:index]
	}
	return path
}

func validNativeID(value string) bool {
	trimmed := strings.TrimSpace(value)
	return value == trimmed && value != "" && len(value) <= 256 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func validText(value string) bool {
	return value != "" && len(value) <= maxNativeTextBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func terminalFromJob(job AgentJob) (productruntime.NativeTerminal, bool) {
	var outcome productruntime.TurnOutcome
	var exit int
	switch job.State {
	case "done":
		outcome = productruntime.TurnCompleted
	case "failed":
		outcome, exit = productruntime.TurnFailed, 1
	case "stopped":
		outcome, exit = productruntime.TurnInterrupted, 130
	default:
		return productruntime.NativeTerminal{}, false
	}
	result := strings.TrimSpace(job.Detail)
	if result == "" && len(job.Output) != 0 {
		keys := make([]string, 0, len(job.Output))
		for key := range job.Output {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var lines []string
		for _, key := range keys {
			lines = append(lines, key+": "+job.Output[key])
		}
		result = strings.Join(lines, "\n")
	}
	if len(result) > maxNativeTextBytes {
		result = result[:maxNativeTextBytes]
	}
	digest := sha256.Sum256([]byte(result))
	return productruntime.NativeTerminal{Outcome: outcome, ExitLike: exit, Result: result, ResultDigest: digest, NativeStopReason: job.State}, true
}
