// Package claudeidentity owns non-secret filesystem and process identity
// helpers for Agent Sessions-managed Claude launch artifacts.
package claudeidentity

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/sessionkey"
	"github.com/antst/agent-sessions/internal/socketpath"
)

// KeyBaselineEntry fingerprints one PID-prefixed native key artifact before a
// gated Claude adapter may create another one.
type KeyBaselineEntry struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// LifecycleRoot returns the current user's private Claude attachment root.
func LifecycleRoot(hostID, sessionID string) string {
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".local", "state")
	}
	return LifecycleRootInState(filepath.Join(root, "agent-sessions", "agents", cleanID(hostID)), sessionID)
}

// LifecycleRootInState returns one Claude attachment root below an explicit state root.
func LifecycleRootInState(stateDir, sessionID string) string {
	return filepath.Join(stateDir, "claude-peers", sessionkey.FromID(sessionID), "config")
}

// LockPath derives the adjacent lifecycle lock path.
func LockPath(lifecycleRoot string) string {
	return filepath.Join(filepath.Dir(lifecycleRoot), "lifecycle.lock")
}

// MessagingSocketPath derives the bounded native Claude socket path.
func MessagingSocketPath(runtimeDir, sessionID string) string {
	return filepath.Join(runtimeDir, "cp-"+sessionkey.FromID(sessionID)+".sock")
}

// ValidateMessagingSocketPath rejects non-canonical or over-budget socket paths.
func ValidateMessagingSocketPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("managed Claude messaging socket path is not absolute and clean")
	}
	if err := socketpath.Validate(path); err != nil {
		return fmt.Errorf("managed Claude messaging socket path is %d bytes; platform limit is %d", len([]byte(path)), socketpath.Limit())
	}
	return nil
}

// NativeSessionTitle returns one unambiguous title from exact native transcripts.
func NativeSessionTitle(configDir, sessionID string) (string, bool) {
	if strings.TrimSpace(sessionID) == "" || filepath.Base(sessionID) != sessionID {
		return "", false
	}
	entries, err := os.ReadDir(filepath.Join(configDir, "projects"))
	if err != nil {
		return "", false
	}
	selected := ""
	for _, project := range entries {
		if !project.IsDir() {
			continue
		}
		path := filepath.Join(configDir, "projects", project.Name(), sessionID+".jsonl")
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil || !info.Mode().IsRegular() {
			return "", false
		}
		title, titleErr := readTranscriptTitle(path, sessionID)
		if titleErr != nil || title == "" || selected != "" && selected != title {
			return "", false
		}
		selected = title
	}
	return selected, selected != ""
}

func readTranscriptTitle(path, expectedSessionID string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // Exact session filename below the configured Claude projects root.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	title := ""
	for scanner.Scan() {
		var event struct {
			Type, SessionID, CustomTitle string
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Type != "custom-title" {
			continue
		}
		if event.SessionID != expectedSessionID {
			return "", errors.New("Claude transcript title identity does not match its session") //nolint:staticcheck // Product name.
		}
		title = strings.TrimSpace(event.CustomTitle)
		if title == "" || len(title) > 512 {
			return "", errors.New("Claude transcript contains an invalid session title") //nolint:staticcheck // Product name.
		}
	}
	return title, scanner.Err()
}

// KeySidecars snapshots strict PID-prefixed Claude service-key artifacts.
func KeySidecars(configRoot string, pid int) ([]KeyBaselineEntry, error) {
	if pid <= 1 {
		return nil, errors.New("invalid Claude peer key baseline PID")
	}
	entries, err := os.ReadDir(filepath.Join(configRoot, "sessions"))
	if err != nil {
		return nil, err
	}
	prefix := strconv.Itoa(pid) + "."
	result := make([]KeyBaselineEntry, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), ".key") {
			continue
		}
		fingerprint, fingerprintErr := keyFingerprint(filepath.Join(configRoot, "sessions", entry.Name()))
		if fingerprintErr != nil {
			return nil, fingerprintErr
		}
		result = append(result, KeyBaselineEntry{Name: entry.Name(), Fingerprint: fingerprint})
	}
	slices.SortFunc(result, func(left, right KeyBaselineEntry) int { return strings.Compare(left.Name, right.Name) })
	return result, nil
}

func keyFingerprint(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("type:%#x", uint32(info.Mode().Type())), nil
	}
	if info.Size() > 4096 {
		return fmt.Sprintf("oversized:%d", info.Size()), nil
	}
	body, err := os.ReadFile(path) //nolint:gosec // Bounded PID-prefixed key below the configured registry.
	if err != nil {
		return "", err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return "", errors.New("native Claude sidecar changed during snapshot")
	}
	digest := sha256.Sum256(body)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}

// ValidateObservedSocketKey binds an observed socket to an exact new native service key.
func ValidateObservedSocketKey(configRoot string, pid int, start, strongStart, socket string, baseline, observed []KeyBaselineEntry) error {
	keyName, err := federation.ClaudeServiceKeyName(pid, socket)
	if err != nil {
		return errors.New("native Claude provisional socket path is invalid")
	}
	owned, err := changedKeySidecars(configRoot, pid, start, strongStart, baseline)
	if err != nil {
		return err
	}
	observedSet := make(map[string]string, len(observed))
	for _, entry := range observed {
		observedSet[entry.Name] = entry.Fingerprint
	}
	for _, key := range owned {
		if key.entry.Name == keyName && observedSet[keyName] == key.entry.Fingerprint {
			return nil
		}
	}
	return errors.New("native Claude provisional socket was not bound to an observed launch key")
}

// ObserveNewKeySidecars returns sidecars created by one exact live adapter.
func ObserveNewKeySidecars(configRoot string, pid int, start, strongStart string, baseline []KeyBaselineEntry) ([]KeyBaselineEntry, error) {
	if strongStart == "" || exactStatus(pid, start, strongStart) != procinfo.Known {
		return nil, errors.New("cannot observe Claude peer keys without an exact live adapter")
	}
	owned, err := changedKeySidecars(configRoot, pid, start, strongStart, baseline)
	if err != nil {
		return nil, err
	}
	if exactStatus(pid, start, strongStart) != procinfo.Known {
		return nil, errors.New("native Claude adapter changed during key observation")
	}
	result := make([]KeyBaselineEntry, 0, len(owned))
	for _, key := range owned {
		result = append(result, key.entry)
	}
	return result, nil
}

// CleanupNewKeySidecars removes only unchanged sidecars owned by the exited adapter.
func CleanupNewKeySidecars(configRoot string, pid int, start, strongStart string, baseline, observed []KeyBaselineEntry, rowBound bool) error {
	owned, err := changedKeySidecars(configRoot, pid, start, strongStart, baseline)
	if err != nil {
		return err
	}
	observedSet := make(map[string]string, len(observed))
	for _, entry := range observed {
		observedSet[entry.Name] = entry.Fingerprint
	}
	for _, key := range owned {
		if fingerprint, ok := observedSet[key.entry.Name]; !rowBound && (!ok || fingerprint != key.entry.Fingerprint) {
			return errors.New("token-only native Claude sidecar was not observed under the exact live adapter")
		}
	}
	for _, key := range owned {
		if !pidAbsent(pid) {
			return errors.New("native Claude PID reappeared before provisional key removal")
		}
		info, statErr := os.Lstat(key.path)
		body, readErr := os.ReadFile(key.path)
		if statErr != nil || readErr != nil || !os.SameFile(key.info, info) || !bytes.Equal(body, key.body) {
			return errors.New("new native Claude sidecar changed before removal")
		}
		if err := os.Remove(key.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

type keyFile struct{ PeerToken, ProcStart, ProcStartFT string }
type ownedKey struct {
	entry KeyBaselineEntry
	path  string
	body  []byte
	info  os.FileInfo
}

//nolint:gocyclo // Exact type, token, process-start, and strong-start checks form one ownership proof.
func changedKeySidecars(configRoot string, pid int, start, strongStart string, baseline []KeyBaselineEntry) ([]ownedKey, error) {
	current, err := KeySidecars(configRoot, pid)
	if err != nil {
		return nil, err
	}
	prior := make(map[string]string, len(baseline))
	for _, entry := range baseline {
		prior[entry.Name] = entry.Fingerprint
	}
	result := make([]ownedKey, 0)
	for _, entry := range current {
		if fingerprint, exists := prior[entry.Name]; exists && fingerprint == entry.Fingerprint {
			continue
		}
		if !validKeyName(pid, entry.Name) {
			continue
		}
		path := filepath.Join(configRoot, "sessions", entry.Name)
		info, statErr := os.Lstat(path)
		body, readErr := os.ReadFile(path) //nolint:gosec // Exact strict sidecar below an attested registry.
		if statErr != nil || readErr != nil || !info.Mode().IsRegular() {
			return nil, errors.New("new native Claude sidecar changed type")
		}
		var key keyFile
		if json.Unmarshal(body, &key) != nil || !validToken(key.PeerToken) || key.ProcStart != start ||
			(key.ProcStartFT != "" && (strongStart == "" || key.ProcStartFT != strongStart)) {
			return nil, errors.New("new native Claude sidecar failed ownership validation")
		}
		result = append(result, ownedKey{entry: entry, path: path, body: body, info: info})
	}
	return result, nil
}

func exactStatus(pid int, start, strongStart string) procinfo.Status {
	info := procinfo.Read(pid)
	if info.Status != procinfo.Known {
		return info.Status
	}
	if info.State == "Z" || info.State == "X" || info.Start != start || strongStart != "" && info.StrongStart != strongStart {
		return procinfo.Absent
	}
	return procinfo.Known
}

func pidAbsent(pid int) bool {
	info := procinfo.Read(pid)
	return info.Status == procinfo.Absent || info.Status == procinfo.Known && (info.State == "Z" || info.State == "X")
}

func validKeyName(pid int, name string) bool {
	prefix := strconv.Itoa(pid) + "."
	digest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".key")
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".key") || len(digest) != 64 {
		return false
	}
	return !strings.ContainsFunc(digest, func(character rune) bool {
		return character < '0' || character > '9' && (character < 'a' || character > 'f')
	})
}

func validToken(token string) bool {
	return len(token) == 32 && token == strings.ToLower(token) && !strings.ContainsFunc(token, func(character rune) bool {
		return character < '0' || character > '9' && (character < 'a' || character > 'f')
	})
}

func cleanID(value string) string {
	var output strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' {
			output.WriteRune(character)
		}
		if output.Len() >= 48 {
			break
		}
	}
	return strings.Trim(output.String(), "_-")
}
