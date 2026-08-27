package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/socketpath"
)

const (
	localControlProtocolVersion = 1
	maxLocalControlFrameBytes   = 2 * 1024 * 1024
)

type controlRole string

const (
	controlRoleAdmin     controlRole = "admin"
	controlRoleLauncher  controlRole = "launcher"
	controlRoleHook      controlRole = "hook"
	controlRoleConnector controlRole = "connector"
	controlRoleService   controlRole = "service"
)

type controlPeerEvidence struct {
	UID         int
	PID         int
	ProcStart   string
	StrongStart string
}

type controlHello struct {
	Type         string      `json:"type"`
	Version      int         `json:"version"`
	RequestID    string      `json:"request_id"`
	Role         controlRole `json:"role"`
	Product      string      `json:"product,omitempty"`
	AttachmentID string      `json:"attachment_id,omitempty"`
	Capability   string      `json:"capability,omitempty"`
}

type controlPrincipal struct {
	Role         controlRole
	Product      string
	AttachmentID string
	SessionID    string
	Peer         controlPeerEvidence
}

type controlRequest struct {
	Type               string          `json:"type"`
	Version            int             `json:"version"`
	RequestID          string          `json:"request_id"`
	Operation          string          `json:"operation"`
	ExpectedGeneration uint64          `json:"expected_generation"`
	ExpectedRevision   string          `json:"expected_revision,omitempty"`
	Payload            json.RawMessage `json:"payload"`
}

type controlSubscribe struct {
	Type               string   `json:"type"`
	Version            int      `json:"version"`
	RequestID          string   `json:"request_id"`
	ExpectedGeneration uint64   `json:"expected_generation"`
	Topics             []string `json:"topics"`
}

type controlResponse struct {
	Type             string          `json:"type"`
	Version          int             `json:"version"`
	RequestID        string          `json:"request_id"`
	Operation        string          `json:"operation"`
	DaemonGeneration uint64          `json:"daemon_generation"`
	Accepted         bool            `json:"accepted"`
	ResourceRevision string          `json:"resource_revision,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	Error            *controlError   `json:"error,omitempty"`
}

type controlServerConfig struct {
	Generation     uint64
	RuntimeVersion string
	OwnerUID       int
	ObservePeer    func(net.Conn) (controlPeerEvidence, error)
	AuthorizeHello func(context.Context, controlPeerEvidence, controlHello) (controlPrincipal, *controlError)
	Dispatch       func(context.Context, controlPrincipal, controlRequest) (controlDispatchResult, *controlError)
	ObserveRequest func(controlObservation)
	ReplayStore    controlReplayStore
}

type controlServer struct{ config controlServerConfig }

func newControlServer(config controlServerConfig) *controlServer {
	if config.OwnerUID == 0 {
		config.OwnerUID = os.Getuid()
	}
	if config.ObservePeer == nil {
		config.ObservePeer = func(connection net.Conn) (controlPeerEvidence, error) {
			unixConnection, ok := connection.(*net.UnixConn)
			if !ok {
				return controlPeerEvidence{}, errors.New("local control connection is not a Unix socket")
			}
			return observeControlPeer(unixConnection)
		}
	}
	if config.ReplayStore == nil {
		config.ReplayStore = newMemoryControlReplayStore()
	}
	return &controlServer{config: config}
}

// serve owns the one accepted-connection loop for an acquired endpoint. Each
// connection is independently framed and attested, but all dispatch remains
// in this process and this daemon generation.
func (server *controlServer) serve(ctx context.Context, endpoint *ownedControlEndpoint) error {
	if endpoint == nil || endpoint.listener == nil {
		return errors.New("local control endpoint is unavailable")
	}
	var sessions sync.WaitGroup
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = endpoint.listener.Close()
		case <-done:
		}
	}()
	defer close(done)
	defer sessions.Wait()
	for {
		connection, err := endpoint.listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept local control connection: %w", err)
		}
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			defer func() { _ = connection.Close() }()
			_ = server.serveConn(ctx, connection)
		}()
	}
}

//nolint:gocyclo // Framing, kernel identity, hello authorization, and correlated dispatch are one security boundary.
func (server *controlServer) serveConn(ctx context.Context, connection net.Conn) error {
	reader := bufio.NewReaderSize(connection, 64*1024)
	helloBody, err := readBoundedControlFrame(reader)
	if err != nil {
		return err
	}
	peer, err := server.config.ObservePeer(connection)
	if err != nil {
		return fmt.Errorf("observe local control peer: %w", err)
	}
	if peer.UID != server.config.OwnerUID {
		return errors.New("local control peer belongs to a different OS user")
	}
	var hello controlHello
	if err := decodeStrictControlFrame(helloBody, &hello); err != nil {
		return fmt.Errorf("decode control hello: %w", err)
	}
	if err := validateControlHello(hello); err != nil {
		return err
	}
	if server.config.AuthorizeHello == nil {
		return errors.New("local control hello authorizer is unavailable")
	}
	principal, failure := server.config.AuthorizeHello(ctx, peer, hello)
	if failure != nil {
		return fmt.Errorf("control hello rejected: %s", failure.Message)
	}
	principal.Peer = peer
	if principal.Role == "" {
		principal.Role = hello.Role
	}
	helloResult := struct {
		Type             string      `json:"type"`
		Version          int         `json:"version"`
		RequestID        string      `json:"request_id"`
		DaemonGeneration uint64      `json:"daemon_generation"`
		RuntimeVersion   string      `json:"runtime_version"`
		Role             controlRole `json:"role"`
		AttachmentID     string      `json:"attachment_id,omitempty"`
		SessionID        string      `json:"session_id,omitempty"`
	}{
		Type: "hello.result", Version: localControlProtocolVersion, RequestID: hello.RequestID,
		DaemonGeneration: server.config.Generation, RuntimeVersion: server.config.RuntimeVersion,
		Role: principal.Role, AttachmentID: principal.AttachmentID, SessionID: principal.SessionID,
	}
	if err := writeControlFrame(connection, helloResult); err != nil {
		return err
	}

	for {
		body, readErr := readBoundedControlFrame(reader)
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return fmt.Errorf("decode local control envelope: %w", err)
		}
		if envelope.Type == "subscribe" {
			if err := server.handleSubscription(connection, principal, body); err != nil {
				return err
			}
			continue
		}
		var request controlRequest
		if err := decodeStrictControlFrame(body, &request); err != nil {
			return fmt.Errorf("decode control request: %w", err)
		}
		if request.Type != "request" || request.Version != localControlProtocolVersion || request.RequestID == "" || request.Operation == "" {
			return errors.New("invalid local control request envelope")
		}
		response, rawReplay := server.handleRequest(ctx, principal, request)
		if len(rawReplay) != 0 {
			if err := writeRawControlFrame(connection, rawReplay); err != nil {
				return err
			}
			continue
		}
		if err := writeControlFrame(connection, response); err != nil {
			return err
		}
	}
}

func (server *controlServer) handleSubscription(connection net.Conn, principal controlPrincipal, body []byte) error {
	var subscription controlSubscribe
	if err := decodeStrictControlFrame(body, &subscription); err != nil {
		return fmt.Errorf("decode control subscription: %w", err)
	}
	request := controlRequest{Type: "request", Version: subscription.Version, RequestID: subscription.RequestID, Operation: "subscribe"}
	if subscription.Type != "subscribe" || subscription.Version != localControlProtocolVersion || subscription.RequestID == "" || len(subscription.Topics) == 0 {
		return writeControlFrame(connection, rejectedControlResponse(request, server.config.Generation, &controlError{
			Code: "invalid_subscription", Message: "subscription envelope or topic inventory is invalid", Retryable: false,
		}))
	}
	if subscription.ExpectedGeneration != server.config.Generation {
		return writeControlFrame(connection, rejectedControlResponse(request, server.config.Generation, &controlError{
			Code: "stale_generation", Message: "subscription generation does not match the running daemon", Retryable: true,
		}))
	}
	topics := append([]string(nil), subscription.Topics...)
	sort.Strings(topics)
	for index, topic := range topics {
		if index > 0 && topic == topics[index-1] {
			return writeControlFrame(connection, rejectedControlResponse(request, server.config.Generation, &controlError{
				Code: "invalid_subscription", Message: "subscription topics must be unique", Retryable: false,
			}))
		}
		if !controlRoleAllowsTopic(principal.Role, topic) {
			return writeControlFrame(connection, rejectedControlResponse(request, server.config.Generation, &controlError{
				Code: "topic_forbidden", Message: "connection role cannot subscribe to this topic", Retryable: false,
			}))
		}
	}
	return writeControlFrame(connection, struct {
		Type             string   `json:"type"`
		Version          int      `json:"version"`
		RequestID        string   `json:"request_id"`
		DaemonGeneration uint64   `json:"daemon_generation"`
		Topics           []string `json:"topics"`
	}{
		Type: "subscribe.result", Version: localControlProtocolVersion, RequestID: subscription.RequestID,
		DaemonGeneration: server.config.Generation, Topics: topics,
	})
}

func (server *controlServer) handleRequest(
	ctx context.Context,
	principal controlPrincipal,
	request controlRequest,
) (response controlResponse, replay json.RawMessage) {
	started := time.Now()
	defer func() {
		if server.config.ObserveRequest == nil {
			return
		}
		observed := response
		if len(replay) != 0 {
			_ = json.Unmarshal(replay, &observed)
		}
		observation := controlObservation{
			RequestID: request.RequestID, Operation: request.Operation, Role: principal.Role,
			Accepted: observed.Accepted, Duration: time.Since(started),
		}
		if observed.Error != nil {
			observation.ErrorCode = observed.Error.Code
			observation.Retryable = observed.Error.Retryable
		}
		server.config.ObserveRequest(observation)
	}()
	if request.ExpectedGeneration != server.config.Generation {
		return rejectedControlResponse(request, server.config.Generation, &controlError{
			Code: "stale_generation", Message: "request generation does not match the running daemon", Retryable: true,
		}), nil
	}
	if !controlRoleAllowsOperation(principal.Role, request.Operation) {
		return rejectedControlResponse(request, server.config.Generation, &controlError{
			Code: "operation_forbidden", Message: "connection role cannot invoke this operation", Retryable: false,
		}), nil
	}
	digest, err := controlPayloadDigest(request.Payload)
	if err != nil {
		return rejectedControlResponse(request, server.config.Generation, &controlError{
			Code: "invalid_payload", Message: "request payload is not valid JSON", Retryable: false,
		}), nil
	}
	key := controlReplayKey{Principal: controlPrincipalReplayNamespace(principal), RequestID: request.RequestID}
	entry, exists, err := server.config.ReplayStore.lookup(ctx, key)
	if err != nil {
		return rejectedControlResponse(request, server.config.Generation, &controlError{
			Code: "replay_unavailable", Message: "idempotency state is unavailable", Retryable: true,
		}), nil
	}
	if exists {
		if entry.Operation != request.Operation || entry.PayloadDigest != digest {
			return rejectedControlResponse(request, server.config.Generation, &controlError{
				Code: "idempotency_conflict", Message: "request id was already used for different work", Retryable: false,
			}), nil
		}
		return controlResponse{}, entry.Response
	}
	if server.config.Dispatch == nil {
		return rejectedControlResponse(request, server.config.Generation, &controlError{
			Code: "operation_unavailable", Message: "operation dispatcher is unavailable", Retryable: true,
		}), nil
	}
	result, failure := server.config.Dispatch(ctx, principal, request)
	if failure != nil {
		return rejectedControlResponse(request, server.config.Generation, failure), nil
	}
	response = controlResponse{
		Type: "response", Version: localControlProtocolVersion, RequestID: request.RequestID,
		Operation: request.Operation, DaemonGeneration: server.config.Generation, Accepted: true,
		ResourceRevision: result.ResourceRevision, Result: result.Result,
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return rejectedControlResponse(request, server.config.Generation, &controlError{Code: "internal", Message: "encode accepted response", Retryable: true}), nil
	}
	if err := server.config.ReplayStore.commit(ctx, key, controlReplayEntry{
		Operation: request.Operation, PayloadDigest: digest, Response: encoded,
	}); err != nil {
		return rejectedControlResponse(request, server.config.Generation, &controlError{
			Code: "replay_commit_failed", Message: "accepted work could not commit its idempotency result", Retryable: true,
		}), nil
	}
	return response, nil
}

func validateControlHello(hello controlHello) error {
	if hello.Type != "hello" || hello.Version != localControlProtocolVersion || hello.RequestID == "" {
		return errors.New("first local control frame must be a compatible hello")
	}
	if _, ok := controlRoleOperations[hello.Role]; !ok {
		return fmt.Errorf("unknown local control role %q", hello.Role)
	}
	if hello.Role == controlRoleConnector && (hello.Product == "" || hello.AttachmentID == "" || hello.Capability == "") {
		return errors.New("connector hello requires product, attachment_id, and capability")
	}
	return nil
}

func decodeStrictControlFrame(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("local control frame contains trailing JSON")
	}
	return nil
}

func readBoundedControlFrame(reader *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		body = append(body, fragment...)
		if len(body) > maxLocalControlFrameBytes+1 {
			return nil, errors.New("local control frame exceeds 2 MiB")
		}
		if err == nil {
			body = body[:len(body)-1]
			if len(body) > maxLocalControlFrameBytes {
				return nil, errors.New("local control frame exceeds 2 MiB")
			}
			return body, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if errors.Is(err, io.EOF) && len(body) == 0 {
				return nil, io.EOF
			}
			return nil, err
		}
	}
}

func writeControlFrame(writer io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeRawControlFrame(writer, body)
}

func writeRawControlFrame(writer io.Writer, body []byte) error {
	if len(body) > maxLocalControlFrameBytes {
		return errors.New("local control response exceeds 2 MiB")
	}
	_, err := writer.Write(append(append([]byte(nil), body...), '\n'))
	return err
}

type controlEndpointOptions struct {
	endpoint      string
	priorIdentity *procinfo.Process
	processInfo   func(int) (procinfo.Info, error)
}

type ownedControlEndpoint struct {
	listener *net.UnixListener
	lock     *os.File
	lockPath string
	lockInfo os.FileInfo
	once     sync.Once
	err      error
}

func controlEndpointLockPath(endpoint string) string { return endpoint + ".lock" }

//nolint:gocyclo // Lock, type, ownership, process identity, and stale replacement gates intentionally remain linear.
func acquireControlEndpoint(options controlEndpointOptions) (*ownedControlEndpoint, error) {
	endpoint := filepath.Clean(options.endpoint)
	if options.endpoint == "" || endpoint != options.endpoint || !filepath.IsAbs(endpoint) {
		return nil, errors.New("local control endpoint must be clean and absolute")
	}
	if err := socketpath.Validate(endpoint); err != nil {
		return nil, err
	}
	parent := filepath.Dir(endpoint)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(parent); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !ownedFileByCurrentUser(info) {
		return nil, errors.New("local control parent is not an owned real directory")
	}
	if err := os.Chmod(parent, 0o700); err != nil { //nolint:gosec // Owner-only socket directory requires execute permission.
		return nil, err
	}
	lockPath := controlEndpointLockPath(endpoint)
	if info, err := os.Lstat(lockPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !ownedFileByCurrentUser(info) {
			return nil, errors.New("local control lock is not an owned regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // adjacent validated owner-only lock.
	if err != nil {
		return nil, err
	}
	cleanupLock := func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}
	if err := lock.Chmod(0o600); err != nil {
		cleanupLock()
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		cleanupLock()
		return nil, fmt.Errorf("another daemon owns the local control lock: %w", err)
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		cleanupLock()
		return nil, err
	}

	if info, statErr := os.Lstat(endpoint); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 || !ownedFileByCurrentUser(info) {
			cleanupLock()
			return nil, errors.New("existing local control endpoint has an unsafe identity")
		}
		if options.priorIdentity == nil {
			cleanupLock()
			return nil, errors.New("existing local control endpoint lacks durable process identity")
		}
		probe := options.processInfo
		if probe == nil {
			probe = func(pid int) (procinfo.Info, error) { return procinfo.Read(pid), nil }
		}
		current, probeErr := probe(options.priorIdentity.PID)
		if probeErr != nil || current.Status == procinfo.Unknown {
			cleanupLock()
			return nil, errors.New("existing local control endpoint owner cannot be corroborated")
		}
		if current.Status == procinfo.Known && current.Start == options.priorIdentity.Start &&
			current.StrongStart == options.priorIdentity.StrongStart {
			cleanupLock()
			return nil, errors.New("existing local control endpoint owner is still live")
		}
		if err := os.Remove(endpoint); err != nil {
			cleanupLock()
			return nil, err
		}
	} else if !os.IsNotExist(statErr) {
		cleanupLock()
		return nil, statErr
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil {
		cleanupLock()
		return nil, err
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		_ = listener.Close()
		cleanupLock()
		return nil, err
	}
	return &ownedControlEndpoint{listener: listener, lock: lock, lockPath: lockPath, lockInfo: lockInfo}, nil
}

// Close releases exactly the listener and adjacent lock identity acquired by this owner.
func (endpoint *ownedControlEndpoint) Close() error {
	endpoint.once.Do(func() {
		if endpoint.listener != nil {
			endpoint.err = endpoint.listener.Close()
		}
		if endpoint.lock != nil {
			if err := syscall.Flock(int(endpoint.lock.Fd()), syscall.LOCK_UN); endpoint.err == nil && err != nil {
				endpoint.err = err
			}
			if err := endpoint.lock.Close(); endpoint.err == nil && err != nil {
				endpoint.err = err
			}
		}
		if current, err := os.Lstat(endpoint.lockPath); err == nil && endpoint.lockInfo != nil && os.SameFile(current, endpoint.lockInfo) {
			_ = os.Remove(endpoint.lockPath)
		}
	})
	return endpoint.err
}

func ownedFileByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}
