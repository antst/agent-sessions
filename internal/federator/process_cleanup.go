package federator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/antst/agent-sessions/internal/procinfo"
)

type exactProcess struct {
	PID         int
	Start       string
	StrongStart string
}

func exactProcessStatus(process exactProcess) procinfo.Status {
	info := procinfo.Read(process.PID)
	if info.Status != procinfo.Known {
		return info.Status
	}
	if info.State == "Z" || info.State == "X" {
		return procinfo.Absent
	}
	if info.Start != process.Start || process.StrongStart != "" && info.StrongStart != process.StrongStart {
		return procinfo.Absent
	}
	return procinfo.Known
}

func pidNamespaceAbsent(pid int) bool {
	info := procinfo.Read(pid)
	return info.Status == procinfo.Absent || info.Status == procinfo.Known && (info.State == "Z" || info.State == "X")
}

func peerAdapterRoot(peer localPeer) exactProcess {
	return exactProcess{PID: peer.PID, Start: peer.ProcStart, StrongStart: peer.AdapterStrongStart}
}

func discoverExactProcessTree(root exactProcess) ([]exactProcess, error) {
	if exactProcessStatus(root) == procinfo.Absent {
		return nil, nil
	}
	processes, err := procinfo.List()
	if err != nil {
		return nil, err
	}
	byPID := make(map[int]procinfo.Process, len(processes))
	owned := map[int]bool{}
	for _, process := range processes {
		byPID[process.PID] = process
	}
	observed, ok := byPID[root.PID]
	if !ok {
		return nil, errors.New("live peer adapter is missing from process snapshot")
	}
	if observed.Start != root.Start || root.StrongStart != "" && observed.StrongStart != root.StrongStart {
		return nil, nil
	}
	owned[root.PID] = true
	for changed := true; changed; {
		changed = false
		for _, process := range processes {
			if !owned[process.PID] && owned[process.Parent] {
				owned[process.PID] = true
				changed = true
			}
		}
	}
	result := make([]exactProcess, 0, len(owned))
	for pid := range owned {
		process := byPID[pid]
		result = append(result, exactProcess{PID: pid, Start: process.Start, StrongStart: process.StrongStart})
	}
	return result, nil
}

// stopPeerAdapterTree freezes the exact native adapter first, repeatedly
// freezes its discovered descendants until the ancestry set is quiescent, and
// only then terminates the retained identities. A child cannot fork across the
// terminal survey, and PID reuse is rechecked before every signal.
//
//nolint:gocyclo // Exact freeze, discovery, signalling, and retry states are deliberately explicit.
func stopPeerAdapterTree(peer localPeer, beforeTerminate func() error) error {
	root := peerAdapterRoot(peer)
	switch exactProcessStatus(root) {
	case procinfo.Absent:
		return nil
	case procinfo.Unknown:
		return errors.New("cannot corroborate peer adapter identity")
	case procinfo.Known:
	}
	if err := syscall.Kill(root.PID, syscall.SIGSTOP); err != nil {
		return err
	}
	known := map[int]exactProcess{root.PID: root}
	completed := false
	defer func() {
		if completed {
			return
		}
		for _, member := range known {
			if exactProcessStatus(member) == procinfo.Known {
				_ = syscall.Kill(member.PID, syscall.SIGCONT)
			}
		}
	}()
	stablePasses := 0
	deadline := time.Now().Add(2 * time.Second)
	for stablePasses < 3 {
		members, err := discoverExactProcessTree(root)
		if err != nil {
			return err
		}
		before := len(known)
		for _, member := range members {
			known[member.PID] = member
		}
		for pid, member := range known {
			switch exactProcessStatus(member) {
			case procinfo.Known:
				if err := syscall.Kill(member.PID, syscall.SIGSTOP); err != nil {
					return err
				}
			case procinfo.Absent:
				delete(known, pid)
			case procinfo.Unknown:
				return fmt.Errorf("cannot corroborate peer adapter descendant %d", pid)
			}
		}
		if len(known) == before {
			stablePasses++
		} else {
			stablePasses = 0
		}
		if time.Now().After(deadline) {
			return errors.New("timed out freezing peer adapter process tree")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if beforeTerminate != nil {
		if err := beforeTerminate(); err != nil {
			return err
		}
	}
	for pid, member := range known {
		if pid != root.PID && exactProcessStatus(member) == procinfo.Known {
			_ = syscall.Kill(member.PID, syscall.SIGKILL)
		}
	}
	if exactProcessStatus(root) == procinfo.Known {
		_ = syscall.Kill(root.PID, syscall.SIGKILL)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		remaining := false
		for _, member := range known {
			if exactProcessStatus(member) == procinfo.Known {
				remaining = true
				break
			}
		}
		if !remaining {
			completed = true
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("timed out stopping peer adapter process tree")
}

func withClaudePeerRetirementLock(peer localPeer, action func() error) error {
	if peer.LifecycleRoot == "" {
		return action()
	}
	lockPath := ClaudePeerLifecycleLockPath(peer.LifecycleRoot)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // deterministic agent-owned session lock.
	if err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("Claude peer profile is attached during retirement") //nolint:staticcheck // Claude is a product name.
	}
	return action()
}

// retirePeerAdapter holds the deterministic profile lock across both process
// retirement and artifact removal. A resume therefore cannot launch in the
// gap between stopping an orphaned adapter and cleaning its PID-bound files.
func retirePeerAdapter(peer localPeer) error {
	return withClaudePeerRetirementLock(peer, func() error {
		adapterOwnedDeath := exactProcessStatus(peerAdapterRoot(peer)) == procinfo.Known
		var observedKeys []ClaudeKeyBaselineEntry
		if adapterOwnedDeath {
			if err := stopPeerAdapterTree(peer, func() error {
				if !peer.ClaudeKeyBaselineSet {
					return nil
				}
				var err error
				observedKeys, err = ObserveClaudePeerNewKeySidecars(
					peer.ClaudeConfigRoot, peer.PID, peer.ProcStart, peer.AdapterStrongStart, peer.ClaudeKeyBaseline,
				)
				return err
			}); err != nil {
				return err
			}
		}
		return cleanupClaudePeerArtifactsLocked(peer, observedKeys)
	})
}

//nolint:gocyclo // Every owned artifact is independently re-attested before removal.
func cleanupClaudePeerArtifactsLocked(peer localPeer, observedKeys []ClaudeKeyBaselineEntry) error {
	if peer.ClaudeConfigRoot == "" {
		return nil
	}
	// A recycled PID is not absence: never unlink its PID-bound socket merely
	// because it no longer matches the old adapter identity.
	if !pidNamespaceAbsent(peer.PID) {
		return errors.New("native Claude PID is not absent after retirement")
	}
	recordPath := filepath.Join(peer.ClaudeConfigRoot, "sessions", strconv.Itoa(peer.PID)+".json")
	recordPresent := false
	body, err := os.ReadFile(recordPath) //nolint:gosec // exact PID row in the configured shared Claude registry.
	if err == nil {
		var record registryRecord
		if json.Unmarshal(body, &record) != nil || record.PID != peer.PID || record.SessionID != peer.SessionID ||
			record.ProcStart != peer.ProcStart || peer.Socket != "" && record.MessagingSocketPath != peer.Socket {
			return errors.New("native Claude record changed before cleanup")
		}
		if peer.Socket == "" {
			peer.Socket = record.MessagingSocketPath
		}
		recordPresent = true
	} else if !os.IsNotExist(err) {
		return err
	}
	keyPath := ""
	keyPresent := false
	if peer.Socket != "" {
		if filepath.Base(peer.Socket) != strconv.Itoa(peer.PID)+".sock" {
			return errors.New("native Claude socket is not PID-bound")
		}
		keyName, keyErr := ClaudeServiceKeyName(peer.PID, peer.Socket)
		if keyErr != nil {
			return errors.New("native Claude peer-token sidecar path is invalid")
		}
		keyPath = filepath.Join(peer.ClaudeConfigRoot, "sessions", keyName)
		if info, statErr := os.Lstat(keyPath); statErr == nil {
			if !info.Mode().IsRegular() {
				return errors.New("native Claude peer-token sidecar changed type")
			}
			keyPresent = true
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
	}
	socketPresent := false
	if peer.Socket != "" {
		if info, err := os.Lstat(peer.Socket); err == nil {
			if info.Mode()&os.ModeSocket == 0 {
				return errors.New("native Claude socket path changed type")
			}
			socketPresent = true
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if socketPresent {
		if !pidNamespaceAbsent(peer.PID) {
			return errors.New("native Claude PID reappeared before socket cleanup")
		}
		if err := os.Remove(peer.Socket); err != nil {
			return err
		}
	}
	if keyPresent && !peer.ClaudeKeyBaselineSet {
		if !pidNamespaceAbsent(peer.PID) {
			return errors.New("native Claude PID reappeared before key removal")
		}
		if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if peer.ClaudeKeyBaselineSet {
		if err := CleanupClaudePeerNewKeySidecars(
			peer.ClaudeConfigRoot, peer.PID, peer.ProcStart, peer.AdapterStrongStart,
			peer.ClaudeKeyBaseline, observedKeys, recordPresent,
		); err != nil {
			return err
		}
	}
	if recordPresent {
		if !pidNamespaceAbsent(peer.PID) {
			return errors.New("native Claude PID reappeared before record cleanup")
		}
		if err := os.Remove(recordPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	settingsPath := filepath.Join(peer.LifecycleRoot, "launch-settings.json")
	if info, err := os.Lstat(settingsPath); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("managed Claude launch settings changed type")
		}
		return os.Remove(settingsPath)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

type claudePeerKeyFile struct {
	PeerToken   string `json:"peerToken"`
	ProcStart   string `json:"procStart"`
	ProcStartFT string `json:"procStartFt"`
}

type claudePeerOwnedKey struct {
	entry ClaudeKeyBaselineEntry
	path  string
	body  []byte
	info  os.FileInfo
}

// ObserveClaudePeerNewKeySidecars fingerprints strictly valid changed keys
// while the exact strong-identity adapter is still live (and, in retirement,
// frozen). A later cleanup may unlink only these unchanged observations.
func ObserveClaudePeerNewKeySidecars(
	configRoot string,
	pid int,
	expectedStart string,
	expectedStrongStart string,
	baseline []ClaudeKeyBaselineEntry,
) ([]ClaudeKeyBaselineEntry, error) {
	adapter := exactProcess{PID: pid, Start: expectedStart, StrongStart: expectedStrongStart}
	if expectedStrongStart == "" || exactProcessStatus(adapter) != procinfo.Known {
		return nil, errors.New("cannot observe Claude peer keys without an exact live adapter")
	}
	owned, err := claudePeerChangedKeySidecars(configRoot, pid, expectedStart, expectedStrongStart, baseline)
	if err != nil {
		return nil, err
	}
	if exactProcessStatus(adapter) != procinfo.Known {
		return nil, errors.New("native Claude adapter changed during key observation")
	}
	result := make([]ClaudeKeyBaselineEntry, 0, len(owned))
	for _, key := range owned {
		result = append(result, key.entry)
	}
	return result, nil
}

// CleanupClaudePeerNewKeySidecars removes only strict PID-bound sidecars that
// appeared after the gated adapter's durable pre-release snapshot. This covers
// native Claude's key-before-row publication order without guessing at stale
// files from an earlier use of the PID.
func CleanupClaudePeerNewKeySidecars(
	configRoot string,
	pid int,
	expectedStart string,
	expectedStrongStart string,
	baseline []ClaudeKeyBaselineEntry,
	observed []ClaudeKeyBaselineEntry,
	rowBound bool,
) error {
	owned, err := claudePeerChangedKeySidecars(configRoot, pid, expectedStart, expectedStrongStart, baseline)
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
		if !pidNamespaceAbsent(pid) {
			return errors.New("native Claude PID reappeared before provisional key removal")
		}
		info, statErr := os.Lstat(key.path)
		body, readErr := os.ReadFile(key.path)
		if statErr != nil || readErr != nil || !os.SameFile(key.info, info) || !bytes.Equal(body, key.body) {
			return errors.New("new native Claude peer-token sidecar changed before removal")
		}
		if err := os.Remove(key.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

//nolint:gocyclo // Every baseline/type/schema/process ownership condition fails closed independently.
func claudePeerChangedKeySidecars(
	configRoot string,
	pid int,
	expectedStart string,
	expectedStrongStart string,
	baseline []ClaudeKeyBaselineEntry,
) ([]claudePeerOwnedKey, error) {
	current, err := ClaudePeerKeySidecars(configRoot, pid)
	if err != nil {
		return nil, err
	}
	baselineSet := make(map[string]string, len(baseline))
	for _, entry := range baseline {
		baselineSet[entry.Name] = entry.Fingerprint
	}
	owned := make([]claudePeerOwnedKey, 0)
	for _, entry := range current {
		if prior, existed := baselineSet[entry.Name]; existed && prior == entry.Fingerprint {
			continue
		}
		name := entry.Name
		if !validClaudePeerKeySidecarName(pid, name) {
			continue
		}
		path := filepath.Join(configRoot, "sessions", name)
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return nil, statErr
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("new native Claude peer-token sidecar changed type")
		}
		body, readErr := os.ReadFile(path) //nolint:gosec // exact strict sidecar below an agent-attested shared registry.
		if readErr != nil {
			return nil, readErr
		}
		var key claudePeerKeyFile
		if json.Unmarshal(body, &key) != nil || !validClaudePeerToken(key.PeerToken) || key.ProcStart != expectedStart ||
			(key.ProcStartFT != "" && (expectedStrongStart == "" || key.ProcStartFT != expectedStrongStart)) {
			return nil, errors.New("new native Claude peer-token sidecar failed ownership validation")
		}
		owned = append(owned, claudePeerOwnedKey{entry: entry, path: path, body: body, info: info})
	}
	return owned, nil
}

func validClaudePeerKeySidecarName(pid int, name string) bool {
	prefix := strconv.Itoa(pid) + "."
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".key") {
		return false
	}
	digest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".key")
	if len(digest) != 64 {
		return false
	}
	for _, character := range digest {
		if character < '0' || character > '9' && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validClaudePeerToken(token string) bool {
	if len(token) != 32 || token != strings.ToLower(token) {
		return false
	}
	return !slices.ContainsFunc([]byte(token), func(character byte) bool {
		return character < '0' || character > '9' && (character < 'a' || character > 'f')
	})
}
