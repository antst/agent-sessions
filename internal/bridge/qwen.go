package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
)

const qwenDualOutputMaxBytes = 16 * 1024 * 1024

var errQwenCleanupIdentityChanged = errors.New("qwen cleanup lifecycle identity changed")

type qwenCleanupIdentityStatus uint8

const (
	qwenCleanupIdentityUnknown qwenCleanupIdentityStatus = iota
	qwenCleanupIdentityExactLive
	qwenCleanupIdentityStopped
	qwenCleanupIdentityRecycled
)

type qwenOwnedArtifact struct {
	Path     string
	identity os.FileInfo
}

type qwenCleanupRequest struct {
	LifecyclePID   int
	LifecycleStart string
	Root           string
	Artifacts      []qwenOwnedArtifact
}

type qwenCleanupDebtError struct {
	Paths []string
}

func (e *qwenCleanupDebtError) Error() string {
	return "Qwen cleanup retained changed artifacts: " + strings.Join(e.Paths, ", ")
}

var qwenCleanupProcessIdentity = observeQwenCleanupProcessIdentity

type qwenAdmissionExpectation struct {
	SessionID       string
	Cwd             string
	Version         string
	ProtocolVersion int
	RequiredEvents  []string
}

type qwenSessionStart struct {
	SessionID       string
	Cwd             string
	Version         string
	ProtocolVersion int
	SupportedEvents []string
}

type qwenEventCursor struct {
	path         string
	identity     os.FileInfo
	offset       int64
	prefixDigest [sha256.Size]byte
}

func qwenRequiredDualOutputEvents() []string {
	return []string{"system", "user", "assistant", "stream_event", "result", "control_request", "control_response"}
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func admitQwenDualOutput(path string, expected qwenAdmissionExpectation) (*qwenEventCursor, qwenSessionStart, error) {
	info, body, err := readAttestedQwenRegularFile(path, nil)
	if err != nil {
		return nil, qwenSessionStart{}, fmt.Errorf("inspect Qwen dual-output path: %w", err)
	}
	newline := bytes.IndexByte(body, '\n')
	if newline < 0 {
		return nil, qwenSessionStart{}, errors.New("qwen dual-output first event is incomplete")
	}
	line := body[:newline]
	var event struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		Data      struct {
			SessionID       string   `json:"session_id"`
			Cwd             string   `json:"cwd"`
			ProtocolVersion int      `json:"protocol_version"`
			Version         string   `json:"version"`
			SupportedEvents []string `json:"supported_events"`
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &event); err != nil {
		return nil, qwenSessionStart{}, fmt.Errorf("decode first Qwen dual-output event: %w", err)
	}
	if event.Type != "system" {
		return nil, qwenSessionStart{}, errors.New("first Qwen dual-output event is not a system event")
	}
	if event.Subtype != "session_start" {
		return nil, qwenSessionStart{}, errors.New("first Qwen system event is not session_start")
	}
	start := qwenSessionStart{
		SessionID: event.Data.SessionID, Cwd: event.Data.Cwd, Version: event.Data.Version,
		ProtocolVersion: event.Data.ProtocolVersion,
		SupportedEvents: append([]string(nil), event.Data.SupportedEvents...),
	}
	if event.SessionID != expected.SessionID || start.SessionID != expected.SessionID || event.SessionID != start.SessionID {
		return nil, qwenSessionStart{}, errors.New("qwen session identity does not match admission")
	}
	if filepath.Clean(start.Cwd) != filepath.Clean(expected.Cwd) || !filepath.IsAbs(start.Cwd) {
		return nil, qwenSessionStart{}, errors.New("qwen working directory does not match admission")
	}
	if start.Version != expected.Version || start.Version == "" {
		return nil, qwenSessionStart{}, errors.New("qwen version does not match admission")
	}
	if start.ProtocolVersion != expected.ProtocolVersion {
		return nil, qwenSessionStart{}, errors.New("qwen dual-output protocol does not match admission")
	}
	for _, required := range expected.RequiredEvents {
		if !slices.Contains(start.SupportedEvents, required) {
			return nil, qwenSessionStart{}, fmt.Errorf("qwen dual-output event inventory omits %q", required)
		}
	}
	consumed := body[:newline+1]
	return &qwenEventCursor{
		path: path, identity: info, offset: int64(len(consumed)), prefixDigest: sha256.Sum256(consumed),
	}, start, nil
}

// Offset returns the number of admitted event-stream bytes consumed by the cursor.
func (c *qwenEventCursor) Offset() int64 {
	if c == nil {
		return 0
	}
	return c.offset
}

// ReadAvailable returns complete newly appended events after re-attesting the stream.
func (c *qwenEventCursor) ReadAvailable() ([]json.RawMessage, error) {
	if c == nil || c.identity == nil {
		return nil, errors.New("qwen event cursor is not initialized")
	}
	info, body, err := readAttestedQwenRegularFile(c.path, c.identity)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) < c.offset {
		return nil, errors.New("qwen dual-output path was truncated")
	}
	if sha256.Sum256(body[:c.offset]) != c.prefixDigest {
		return nil, errors.New("qwen dual-output body changed before the cursor")
	}
	tail := body[c.offset:]
	lastNewline := bytes.LastIndexByte(tail, '\n')
	if lastNewline < 0 {
		return nil, nil
	}
	complete := tail[:lastNewline+1]
	lines := bytes.Split(complete, []byte{'\n'})
	result := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !json.Valid(line) {
			return nil, errors.New("qwen dual-output contains malformed JSON")
		}
		result = append(result, append(json.RawMessage(nil), line...))
	}
	c.offset += int64(len(complete))
	c.prefixDigest = sha256.Sum256(body[:c.offset])
	c.identity = info
	return result, nil
}

type qwenInputWriter struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	identity os.FileInfo
	offset   int64
	digest   [sha256.Size]byte
}

func openQwenInputWriter(path string) (*qwenInputWriter, error) {
	info, body, err := readAttestedQwenRegularFile(path, nil)
	if err != nil {
		return nil, fmt.Errorf("inspect Qwen input path: %w", err)
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("qwen input path mode is %04o, want 0600", info.Mode().Perm())
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // The exact regular-file identity is re-attested immediately after opening.
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, errors.New("qwen input path changed while opening")
	}
	return &qwenInputWriter{
		path: path, file: file, identity: info, offset: int64(len(body)), digest: sha256.Sum256(body),
	}, nil
}

// Offset returns the exact admitted input length tracked by the writer.
func (w *qwenInputWriter) Offset() int64 {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.offset
}

// Submit appends one framed input after re-attesting the exact input artifact.
func (w *qwenInputWriter) Submit(text string) error {
	if w == nil || w.file == nil {
		return errors.New("qwen input writer is closed")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("qwen input submit text is empty")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, body, err := readAttestedQwenRegularFile(w.path, w.identity)
	if err != nil {
		return err
	}
	if int64(len(body)) != w.offset {
		return errors.New("qwen input body cursor changed")
	}
	if sha256.Sum256(body) != w.digest {
		return errors.New("qwen input body changed before append")
	}
	opened, err := w.file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(w.identity, opened) {
		return errors.New("qwen input descriptor identity changed")
	}
	record, err := json.Marshal(map[string]string{"type": "submit", "text": text})
	if err != nil {
		return err
	}
	record = append(record, '\n')
	if _, err := w.file.Write(record); err != nil {
		return err
	}
	if err := w.file.Sync(); err != nil {
		return err
	}
	expected := append(append([]byte(nil), body...), record...)
	info, after, err := readAttestedQwenRegularFile(w.path, w.identity)
	if err != nil {
		return err
	}
	if !bytes.Equal(after, expected) {
		return errors.New("qwen input append could not be re-attested")
	}
	w.identity, w.offset, w.digest = info, int64(len(after)), sha256.Sum256(after)
	return nil
}

// Close closes the private input descriptor and prevents further submissions.
func (w *qwenInputWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func readAttestedQwenRegularFile(path string, expected os.FileInfo) (os.FileInfo, []byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("path is not a regular non-symlink file: %s", info.Mode())
	}
	if expected != nil && !os.SameFile(expected, info) {
		return nil, nil, errors.New("path identity changed")
	}
	if info.Size() > qwenDualOutputMaxBytes {
		return nil, nil, errors.New("qwen protocol file exceeds the bounded size")
	}
	file, err := os.Open(path) //nolint:gosec // exact path is lstat-attested immediately before open and fstat-checked below.
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, nil, errors.New("path changed while opening")
	}
	body, err := io.ReadAll(io.LimitReader(file, qwenDualOutputMaxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > qwenDualOutputMaxBytes {
		return nil, nil, errors.New("qwen protocol file exceeds the bounded size")
	}
	closed, err := os.Lstat(path)
	if err != nil || !closed.Mode().IsRegular() || closed.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, closed) || closed.Size() != int64(len(body)) {
		return nil, nil, errors.New("path changed while reading")
	}
	return closed, body, nil
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func observeQwenOwnedArtifacts(root string, paths []string) ([]qwenOwnedArtifact, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || !filepath.IsAbs(root) || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() || rootInfo.Mode().Perm() != 0o700 {
		return nil, errors.New("qwen ownership root is not an exact private directory")
	}
	result := make([]qwenOwnedArtifact, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		path = filepath.Clean(path)
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
			!filepath.IsAbs(path) || seen[path] {
			return nil, errors.New("qwen artifact is outside its ownership root or duplicated")
		}
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return nil, fmt.Errorf("qwen artifact %s is not an exact private regular file", path)
		}
		seen[path] = true
		result = append(result, qwenOwnedArtifact{Path: path, identity: info})
	}
	return result, nil
}

func observeQwenCleanupProcessIdentity(pid int, expected string) qwenCleanupIdentityStatus {
	if pid <= 1 || expected == "" {
		return qwenCleanupIdentityUnknown
	}
	probe := probeProcessIdentity(pid)
	switch probe.status {
	case processIdentityProbeAbsent:
		return qwenCleanupIdentityStopped
	case processIdentityProbeUnknown:
		return qwenCleanupIdentityUnknown
	case processIdentityProbeKnown:
		if strings.HasPrefix(probe.state, "Z") || strings.HasPrefix(probe.state, "X") {
			if probe.start == "" || probe.start == expected {
				return qwenCleanupIdentityStopped
			}
			return qwenCleanupIdentityRecycled
		}
		if probe.start == expected {
			return qwenCleanupIdentityExactLive
		}
		if probe.start != "" {
			return qwenCleanupIdentityRecycled
		}
	}
	return qwenCleanupIdentityUnknown
}

func cleanupQwenOwnedArtifacts(request qwenCleanupRequest) error {
	return cleanupQwenOwnedArtifactsWithContext(context.Background(), request)
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func cleanupQwenOwnedArtifactsWithContext(_ context.Context, request qwenCleanupRequest) error {
	status := qwenCleanupProcessIdentity(request.LifecyclePID, request.LifecycleStart)
	switch status {
	case qwenCleanupIdentityStopped:
	case qwenCleanupIdentityRecycled:
		return errQwenCleanupIdentityChanged
	case qwenCleanupIdentityExactLive:
		return errors.New("qwen cleanup lifecycle process is still live")
	case qwenCleanupIdentityUnknown:
		return errors.New("qwen cleanup lifecycle identity is unknown")
	}
	rootInfo, err := os.Lstat(request.Root)
	if err != nil || !filepath.IsAbs(request.Root) || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() || rootInfo.Mode().Perm() != 0o700 {
		return errors.New("qwen cleanup ownership root changed")
	}
	debt := []string{}
	for _, artifact := range request.Artifacts {
		path := filepath.Clean(artifact.Path)
		relative, relErr := filepath.Rel(request.Root, path)
		if relErr != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || artifact.identity == nil {
			debt = append(debt, path)
			continue
		}
		info, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !os.SameFile(info, artifact.identity) {
			debt = append(debt, path)
			continue
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			debt = append(debt, path)
		}
	}
	if len(debt) != 0 {
		sort.Strings(debt)
		return &qwenCleanupDebtError{Paths: debt}
	}
	return nil
}
