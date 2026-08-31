package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
)

func TestDaemonFederationOwnsExactlyOneHostLoop(t *testing.T) {
	component, err := NewFederation(federation.EmbeddedHostOptions{
		HostID: "host-a", HostName: "host-a",
		Snapshot: func(context.Context) ([]federation.Peer, error) { return nil, nil },
		Deliver:  func(context.Context, federation.Peer, federation.Peer, federation.AgentFrame) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- component.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for !component.running.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := component.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second federation authority result = %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon federation did not stop")
	}
}
