package claudeidentity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagedClaudePathsAreDeterministicAndSocketBounded(t *testing.T) {
	state := t.TempDir()
	first := LifecycleRootInState(state, "session-a")
	if first != LifecycleRootInState(state, "session-a") || first == LifecycleRootInState(state, "session-b") {
		t.Fatal("Claude lifecycle root is not deterministic and session-scoped")
	}
	socket := MessagingSocketPath(t.TempDir(), "session-a")
	if err := ValidateMessagingSocketPath(socket); err != nil {
		t.Fatalf("managed socket path: %v", err)
	}
}

func TestClaudeKeySidecarSnapshotRejectsReplacementIdentity(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "4242.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.key"
	path := filepath.Join(sessions, name)
	if err := os.WriteFile(path, []byte(`{"peerToken":"0123456789abcdef0123456789abcdef","procStart":"start"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := KeySidecars(root, 4242)
	if err != nil || len(baseline) != 1 || baseline[0].Name != name {
		t.Fatalf("key baseline = %#v, %v", baseline, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"peerToken":"fedcba9876543210fedcba9876543210","procStart":"start"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := KeySidecars(root, 4242)
	if err != nil || len(current) != 1 || current[0].Fingerprint == baseline[0].Fingerprint {
		t.Fatalf("replacement was not distinguished: before=%#v after=%#v err=%v", baseline, current, err)
	}
}
