//go:build linux || darwin

package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/antst/agent-sessions/internal/procinfo"
)

type toolRootProcessIdentity struct {
	PID         int    `json:"pid"`
	ProcStart   string `json:"procStart"`
	StrongStart string `json:"strongStart"`
}

type toolRootLedgerConfig struct {
	Version          int
	Product          string
	ManagerIdentity  toolRootProcessIdentity
	WorkerIdentity   toolRootProcessIdentity
	CapabilityDigest string
	IntentRevision   string
	Root             string
	ObserveProcess   func(int) procinfo.Info
	RetireRoot       func(toolRootProcessIdentity) error
}

type toolRootLedgerState struct {
	Version          int                       `json:"version"`
	Product          string                    `json:"product"`
	ManagerIdentity  toolRootProcessIdentity   `json:"managerIdentity"`
	WorkerIdentity   toolRootProcessIdentity   `json:"workerIdentity"`
	CapabilityDigest string                    `json:"capabilityDigest"`
	IntentRevision   string                    `json:"intentRevision"`
	AdmissionOpen    bool                      `json:"admissionOpen"`
	CleanupState     string                    `json:"cleanupState"`
	Roots            []toolRootProcessIdentity `json:"roots,omitempty"`
}

type toolRootLedger struct {
	config    toolRootLedgerConfig
	statePath string
	lockPath  string
}

func prepareToolRootLedger(config toolRootLedgerConfig) (*toolRootLedger, error) {
	if err := validateToolRootLedgerConfig(config); err != nil {
		return nil, err
	}
	if err := ensurePrivateToolRootDirectory(config.Root); err != nil {
		return nil, err
	}
	ledger := newToolRootLedger(config)
	state := toolRootLedgerState{
		Version: config.Version, Product: config.Product,
		ManagerIdentity: config.ManagerIdentity, WorkerIdentity: config.WorkerIdentity,
		CapabilityDigest: config.CapabilityDigest, IntentRevision: config.IntentRevision,
		AdmissionOpen: true, CleanupState: "pending",
	}
	if err := ledger.withLock(func() error {
		if _, err := os.Lstat(ledger.statePath); err == nil {
			return errors.New("tool-root ledger already exists")
		} else if !os.IsNotExist(err) {
			return err
		}
		return writeJSONAtomic(ledger.statePath, state)
	}); err != nil {
		return nil, err
	}
	return ledger, nil
}

func openToolRootLedger(config toolRootLedgerConfig) (*toolRootLedger, error) {
	if err := validateToolRootLedgerConfig(config); err != nil {
		return nil, err
	}
	ledger := newToolRootLedger(config)
	if _, err := ledger.readState(); err != nil {
		return nil, err
	}
	return ledger, nil
}

func newToolRootLedger(config toolRootLedgerConfig) *toolRootLedger {
	return &toolRootLedger{
		config: config, statePath: filepath.Join(config.Root, "ledger.json"),
		lockPath: filepath.Join(config.Root, "ledger.lock"),
	}
}

func validateToolRootLedgerConfig(config toolRootLedgerConfig) error {
	if config.Version <= 0 || config.Product == "" || config.IntentRevision == "" ||
		len(config.CapabilityDigest) != 64 || strings.Trim(config.CapabilityDigest, "0123456789abcdef") != "" ||
		!filepath.IsAbs(config.Root) || !validToolRootIdentity(config.ManagerIdentity) ||
		!validToolRootIdentity(config.WorkerIdentity) || config.ObserveProcess == nil || config.RetireRoot == nil {
		return errors.New("invalid tool-root ledger configuration")
	}
	return nil
}

func ensurePrivateToolRootDirectory(root string) error {
	if info, err := os.Lstat(root); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
			return errors.New("tool-root ledger directory is not private")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return os.Chmod(root, 0o700) //nolint:gosec // Private directory needs owner-only search permission.
}

func (ledger *toolRootLedger) register(root toolRootProcessIdentity) error {
	if !validToolRootIdentity(root) {
		return errors.New("invalid detached tool-root identity")
	}
	return ledger.withLock(func() error {
		state, err := ledger.readState()
		if err != nil {
			return err
		}
		if !state.AdmissionOpen {
			return errors.New("tool-root admission is closed")
		}
		if !knownToolRootIdentity(ledger.config.ObserveProcess(root.PID), root) {
			return errors.New("detached tool-root identity is not live")
		}
		for index, existing := range state.Roots {
			if existing.PID != root.PID {
				continue
			}
			if existing == root {
				return nil
			}
			if knownToolRootIdentity(ledger.config.ObserveProcess(existing.PID), existing) {
				return errors.New("live detached tool-root PID is already owned")
			}
			state.Roots[index] = root
			state.CleanupState = "pending"
			return ledger.writeState(state)
		}
		state.Roots = append(state.Roots, root)
		slices.SortFunc(state.Roots, func(left, right toolRootProcessIdentity) int { return left.PID - right.PID })
		state.CleanupState = "pending"
		return ledger.writeState(state)
	})
}

func (ledger *toolRootLedger) closeAdmission() error {
	return ledger.withLock(func() error {
		state, err := ledger.readState()
		if err != nil {
			return err
		}
		state.AdmissionOpen = false
		return ledger.writeState(state)
	})
}

func (ledger *toolRootLedger) reconcileCleanup() error {
	return ledger.withLock(func() error {
		state, err := ledger.readState()
		if err != nil {
			return err
		}
		state.AdmissionOpen = false
		remaining := make([]toolRootProcessIdentity, 0, len(state.Roots))
		var cleanupErr error
		for _, root := range state.Roots {
			observed := ledger.config.ObserveProcess(root.PID)
			switch {
			case observed.Status == procinfo.Absent:
				continue
			case observed.Status == procinfo.Known && !knownToolRootIdentity(observed, root):
				// PID reuse proves the recorded root is gone. Never signal the
				// unrelated incarnation now occupying that namespace.
				continue
			case observed.Status != procinfo.Known:
				remaining = append(remaining, root)
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("detached tool-root %d identity is unknown", root.PID))
				continue
			}
			if err := ledger.config.RetireRoot(root); err != nil {
				remaining = append(remaining, root)
				cleanupErr = errors.Join(cleanupErr, err)
				continue
			}
			after := ledger.config.ObserveProcess(root.PID)
			if after.Status == procinfo.Known && knownToolRootIdentity(after, root) || after.Status == procinfo.Unknown {
				remaining = append(remaining, root)
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("detached tool-root %d survived retirement", root.PID))
			}
		}
		state.Roots = remaining
		state.CleanupState = "clean"
		if len(remaining) != 0 || cleanupErr != nil {
			state.CleanupState = "debt"
		}
		if err := ledger.writeState(state); err != nil {
			return errors.Join(cleanupErr, err)
		}
		return cleanupErr
	})
}

func (ledger *toolRootLedger) snapshot() (toolRootLedgerState, error) {
	var snapshot toolRootLedgerState
	err := ledger.withLock(func() error {
		state, err := ledger.readState()
		if err == nil {
			state.Roots = append([]toolRootProcessIdentity(nil), state.Roots...)
			snapshot = state
		}
		return err
	})
	return snapshot, err
}

func (ledger *toolRootLedger) readState() (toolRootLedgerState, error) {
	body, err := os.ReadFile(ledger.statePath)
	if err != nil {
		return toolRootLedgerState{}, err
	}
	var state toolRootLedgerState
	if jsonErr := json.Unmarshal(body, &state); jsonErr != nil || !ledger.validState(state) {
		return toolRootLedgerState{}, errors.New("invalid durable tool-root ledger")
	}
	return state, nil
}

func (ledger *toolRootLedger) writeState(state toolRootLedgerState) error {
	if !ledger.validState(state) {
		return errors.New("refuse invalid durable tool-root ledger")
	}
	return writeJSONAtomic(ledger.statePath, state)
}

func (ledger *toolRootLedger) validState(state toolRootLedgerState) bool {
	if state.Version != ledger.config.Version || state.Product != ledger.config.Product ||
		state.ManagerIdentity != ledger.config.ManagerIdentity || state.WorkerIdentity != ledger.config.WorkerIdentity ||
		state.CapabilityDigest != ledger.config.CapabilityDigest || state.IntentRevision != ledger.config.IntentRevision ||
		!containsString([]string{"pending", "debt", "clean"}, state.CleanupState) {
		return false
	}
	seen := map[int]bool{}
	for _, root := range state.Roots {
		if !validToolRootIdentity(root) || seen[root.PID] {
			return false
		}
		seen[root.PID] = true
	}
	return true
}

func (ledger *toolRootLedger) withLock(operation func() error) error {
	lock, err := os.OpenFile(ledger.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	return operation()
}

func validToolRootIdentity(identity toolRootProcessIdentity) bool {
	return identity.PID > 1 && identity.ProcStart != "" && identity.StrongStart != ""
}

func knownToolRootIdentity(info procinfo.Info, identity toolRootProcessIdentity) bool {
	return info.Status == procinfo.Known && info.Start == identity.ProcStart && info.StrongStart == identity.StrongStart
}
