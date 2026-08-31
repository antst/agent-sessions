package bridge

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type cleanupStats struct {
	RegistryFiles int
	StateFiles    int
	SocketFiles   int
}

// reconcileDeadConnectorArtifacts keeps current connector ownership checks
// together so dead transport reconciliation cannot drift away from exact
// process-identity validation.
func reconcileDeadConnectorArtifacts(paths nativePaths) cleanupStats {
	runtimeRoot := filepath.Dir(paths.supervisorSock)
	registryRoot := filepath.Join(paths.claudeRoot, "sessions")
	stats := cleanupStats{}
	sessionsRoot := filepath.Join(paths.dataRoot, "sessions")
	if entries, err := os.ReadDir(sessionsRoot); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			statePath := filepath.Join(sessionsRoot, entry.Name(), "state.json")
			removedState, removedRegistry, removedSockets := cleanupStaleStateFile(statePath, runtimeRoot, registryRoot)
			if removedState {
				stats.StateFiles++
			}
			if removedRegistry {
				stats.RegistryFiles++
			}
			stats.SocketFiles += removedSockets
		}
	}
	if entries, err := os.ReadDir(registryRoot); err == nil {
		for _, entry := range entries {
			pid, ok := registryPID(entry.Name())
			if !ok {
				continue
			}
			file := filepath.Join(registryRoot, entry.Name())
			row := readJSONMap(file)
			if !ownedBridgeRegistry(row, pid, runtimeRoot) ||
				cleanupProcessIdentityStatus(pid, stringValue(row["procStart"])).Status != processIdentityStale {
				continue
			}
			removeJSONIf(file, func(current map[string]any) bool {
				return ownedBridgeRegistry(current, pid, runtimeRoot) &&
					cleanupProcessIdentityStatus(pid, stringValue(current["procStart"])).Status == processIdentityStale
			})
			if _, err := os.Lstat(file); os.IsNotExist(err) {
				stats.RegistryFiles++
			}
		}
	}
	return stats
}

func cleanupStaleStateFile(statePath, runtimeRoot, registryRoot string) (bool, bool, int) {
	state := readJSONMap(statePath)
	pid := intValue(state["pid"])
	if !ownedBridgeState(state, statePath, runtimeRoot, registryRoot) ||
		cleanupProcessIdentityStatus(pid, stringValue(state["procStart"])).Status != processIdentityStale {
		return false, false, 0
	}
	removedSockets := 0
	backend := stringValue(state["backendSocketPath"])
	stable := stringValue(state["socketPath"])
	if removeOwnedStableAlias(stable, backend, runtimeRoot) {
		removedSockets++
	}
	if os.Remove(backend) == nil {
		removedSockets++
	}
	registry := stringValue(state["registryFile"])
	_, registryBefore := os.Lstat(registry)
	removeJSONIf(registry, func(row map[string]any) bool {
		return ownedBridgeRegistry(row, pid, runtimeRoot) &&
			stringValue(row["sessionId"]) == stringValue(state["sessionId"]) &&
			samePath(stringValue(row["messagingSocketPath"]), stable)
	})
	_, registryErr := os.Lstat(registry)
	removeJSONIf(statePath, func(current map[string]any) bool {
		return ownedBridgeState(current, statePath, runtimeRoot, registryRoot) &&
			intValue(current["pid"]) == pid && stringValue(current["sessionId"]) == stringValue(state["sessionId"])
	})
	_, err := os.Lstat(statePath)
	return os.IsNotExist(err), registryBefore == nil && os.IsNotExist(registryErr), removedSockets
}

func ownedBridgeState(state map[string]any, statePath, runtimeRoot, registryRoot string) bool {
	if state == nil {
		return false
	}
	pid := intValue(state["pid"])
	sessionID := stringValue(state["sessionId"])
	if pid <= 0 || !validSessionID(sessionID) {
		return false
	}
	key := sessionKey(sessionID)
	backend := stringValue(state["backendSocketPath"])
	stable := stringValue(state["socketPath"])
	currentSocket := filepath.Join(runtimeRoot, "session-"+key+".sock")
	validSocketShape := samePath(stable, currentSocket) && samePath(backend, currentSocket)
	return filepath.Base(statePath) == "state.json" && filepath.Base(filepath.Dir(statePath)) == key &&
		validSocketShape &&
		samePath(stringValue(state["registryFile"]), filepath.Join(registryRoot, strconv.Itoa(pid)+".json"))
}

func removeOwnedStableAlias(stable, backend, runtimeRoot string) bool {
	if !ownedRuntimePath(stable, runtimeRoot) || !ownedRuntimePath(backend, runtimeRoot) {
		return false
	}
	target, err := os.Readlink(stable)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(stable), target)
	}
	target, _ = filepath.Abs(target)
	backend, _ = filepath.Abs(backend)
	return target == backend && os.Remove(stable) == nil
}

func ownedBridgeRegistry(row map[string]any, pid int, runtimeRoot string) bool {
	entrypoint := stringValue(row["entrypoint"])
	return row != nil && intValue(row["pid"]) == pid &&
		(entrypoint == "codex" || entrypoint == "grok" || entrypoint == "qwen") &&
		strings.HasPrefix(stringValue(row["version"]), "agent-sessions/") &&
		ownedRuntimePath(stringValue(row["messagingSocketPath"]), runtimeRoot)
}

func ownedRuntimePath(path, runtimeRoot string) bool {
	if path == "" || runtimeRoot == "" {
		return false
	}
	path, pathErr := filepath.Abs(path)
	root, rootErr := filepath.Abs(runtimeRoot)
	if pathErr != nil || rootErr != nil {
		return false
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func samePath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	left, leftErr := filepath.Abs(left)
	right, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(left) == filepath.Clean(right)
}

func registryPID(name string) (int, bool) {
	if filepath.Ext(name) != ".json" {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
	return pid, err == nil && pid > 0
}
