package federator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
func stopPeerAdapterTree(peer localPeer) error {
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
		if exactProcessStatus(peerAdapterRoot(peer)) == procinfo.Known {
			if err := stopPeerAdapterTree(peer); err != nil {
				return err
			}
		}
		return cleanupClaudePeerArtifactsLocked(peer)
	})
}

//nolint:gocyclo // Every owned artifact is independently re-attested before removal.
func cleanupClaudePeerArtifactsLocked(peer localPeer) error {
	if peer.ClaudeConfigRoot == "" {
		return nil
	}
	// A recycled PID is not absence: never unlink its PID-bound socket merely
	// because it no longer matches the old adapter identity.
	if procinfo.Read(peer.PID).Status != procinfo.Absent {
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
		if procinfo.Read(peer.PID).Status != procinfo.Absent {
			return errors.New("native Claude PID reappeared before socket cleanup")
		}
		if err := os.Remove(peer.Socket); err != nil {
			return err
		}
	}
	if keyPresent {
		if procinfo.Read(peer.PID).Status != procinfo.Absent {
			return errors.New("native Claude PID reappeared before key removal")
		}
		if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if recordPresent {
		if procinfo.Read(peer.PID).Status != procinfo.Absent {
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
