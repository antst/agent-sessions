package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/federator"
)

// RunPeer dispatches a generic exact-session resume through the product stored
// in the host agent's durable session catalog.
func RunPeer(args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Print(peerUsage())
		return nil
	}
	if len(args) < 2 || args[0] != "resume" || strings.TrimSpace(args[1]) == "" {
		return usageError(peerUsage())
	}
	sessionID := args[1]
	resolved, err := federator.LookupSessionPreferences(agentRuntimeDir(), sessionID)
	if err != nil {
		return fmt.Errorf("look up session %s: %w", sessionID, err)
	}
	launcherName, productArgs, err := genericResumeInvocation(resolved.Preference.Product, resolved.Preference.Kind, sessionID, args[2:])
	if err != nil {
		return err
	}
	launcherPath, err := siblingOrPathExecutable(launcherName)
	if err != nil {
		return err
	}
	return Exec(launcherPath, productArgs, nil)
}

func genericResumeInvocation(product, kind, sessionID string, extra []string) (string, []string, error) {
	var launcherName string
	args := make([]string, 0, len(extra)+2)
	switch kind + ":" + product {
	case federator.SessionKindInteractive + ":codex":
		launcherName = "codex-peer"
		args = append(args, "resume", sessionID)
	case federator.SessionKindInteractive + ":claude":
		launcherName = "claude-peer"
		args = append(args, "--resume", sessionID)
	case federator.SessionKindInteractive + ":grok":
		launcherName = "grok-peer"
		args = append(args, "--resume", sessionID)
	case federator.SessionKindLane + ":codex":
		launcherName = "codex-peer-lane"
		args = append(args, "resume", sessionID)
	case federator.SessionKindLane + ":claude":
		launcherName = "claude-peer-lane"
		args = append(args, "resume", sessionID)
	case federator.SessionKindLane + ":grok":
		launcherName = "grok-peer-lane"
		args = append(args, "resume", sessionID)
	default:
		return "", nil, fmt.Errorf("session %s has unsupported resume kind %q for product %q", sessionID, kind, product)
	}
	return launcherName, append(args, extra...), nil
}

func siblingOrPathExecutable(name string) (string, error) {
	if current, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(current), name)
		if info, statErr := os.Stat(candidate); statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", &ExitError{Code: 127, Err: errors.New(name + " was not found beside peer or on PATH")}
	}
	return path, nil
}

func peerUsage() string {
	return "Usage: peer resume SESSION_UUID [PRODUCT_OPTIONS...]\n"
}
