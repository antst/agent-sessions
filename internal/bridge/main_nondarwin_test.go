//go:build !darwin

package bridge

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const bridgeTestRootEnv = "AGENT_SESSIONS_BRIDGE_TEST_ROOT"

// TestMain gives the bridge suite and every helper subprocess an isolated
// state and runtime namespace. Helper subprocesses inherit the root but leave
// cleanup to the top-level test process.
func TestMain(m *testing.M) {
	if inherited := os.Getenv(bridgeTestRootEnv); inherited != "" {
		// Preserve any narrower state/runtime overrides inherited from the
		// individual test that launched this helper process.
		os.Exit(m.Run())
	}

	root, err := os.MkdirTemp("", "agent-sessions-bridge-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv(bridgeTestRootEnv, root); err != nil {
		_ = os.RemoveAll(root)
		panic(err)
	}
	setBridgeTestRoots(root)
	code := m.Run()
	if err := os.RemoveAll(root); err != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "remove bridge test root: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

func setBridgeTestRoots(root string) {
	stateRoot := filepath.Join(root, "state")
	runtimeRoot := filepath.Join(root, "run")
	if err := os.MkdirAll(stateRoot, 0700); err != nil {
		panic(err)
	}
	if err := os.MkdirAll(runtimeRoot, 0700); err != nil {
		panic(err)
	}
	if err := os.Setenv("AGENT_SESSIONS_STATE_ROOT", stateRoot); err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_RUNTIME_DIR", runtimeRoot); err != nil {
		panic(err)
	}
	if err := os.Setenv("AGENT_SESSIONS_COMPACT_RUNTIME_DIR", filepath.Join(root, "compact")); err != nil {
		panic(err)
	}
}
