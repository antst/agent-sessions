package launcher

import (
	"errors"
	"testing"
)

func TestPeerResumeFailsBeforeCatalogLookupWhenDaemonUnavailable(t *testing.T) {
	previous := requirePeerDaemon
	t.Cleanup(func() { requirePeerDaemon = previous })
	want := errors.New("daemon unavailable")
	requirePeerDaemon = func() error { return want }
	if err := RunPeer([]string{"resume", "00000000-0000-0000-0000-000000000001"}); !errors.Is(err, want) {
		t.Fatalf("RunPeer error = %v, want %v", err, want)
	}
}

func TestPeerHelpDoesNotRequireDaemon(t *testing.T) {
	previous := requirePeerDaemon
	t.Cleanup(func() { requirePeerDaemon = previous })
	requirePeerDaemon = func() error {
		t.Fatal("peer help queried daemon")
		return nil
	}
	if err := RunPeer([]string{"--help"}); err != nil {
		t.Fatal(err)
	}
}
