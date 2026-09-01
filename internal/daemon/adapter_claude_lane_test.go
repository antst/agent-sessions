package daemon

import (
	"context"
	"reflect"
	"testing"
)

func TestClaudeLaneAdapterBuildsExactWorkerTranscriptAndPermissionArguments(t *testing.T) {
	adapter := NewClaudeLaneAdapter()
	started, err := adapter.Command(ClaudeLaneRequest{
		LaneID: "lane", Name: "worker", Prompt: "start", PermissionMode: "dontAsk", Arguments: []string{"--model", "opus"},
	})
	wantStart := []string{"-p", "--output-format", "json", "--name", "worker", "--session-id", "lane", "--permission-mode", "dontAsk", "--model", "opus", "start"}
	if err != nil || !reflect.DeepEqual(started.Arguments, wantStart) {
		t.Fatalf("start command = %#v, %v", started.Arguments, err)
	}
	resumed, err := adapter.Command(ClaudeLaneRequest{
		LaneID: "lane", NativeSession: "transcript", Name: "worker", Prompt: "resume", PermissionMode: "bypassPermissions", Resume: true,
	})
	wantResume := []string{"-p", "--output-format", "json", "--name", "worker", "--resume", "transcript", "--permission-mode", "bypassPermissions", "resume"}
	if err != nil || !reflect.DeepEqual(resumed.Arguments, wantResume) {
		t.Fatalf("resume command = %#v, %v", resumed.Arguments, err)
	}
}

func TestClaudeLaneAdapterAllowsOneStreamWorkerAndInterruptsBeforeArchive(t *testing.T) {
	adapter := NewClaudeLaneAdapter()
	ctx, cancel := context.WithCancel(context.Background())
	if err := adapter.Register("lane", cancel); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Register("lane", func() {}); err == nil {
		t.Fatal("duplicate claude stream worker was accepted")
	}
	if err := adapter.Archive("lane"); err == nil {
		t.Fatal("active claude stream worker was archived")
	}
	if err := adapter.Interrupt("lane"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("claude interrupt did not cancel worker")
	}
	adapter.Complete("lane")
	if err := adapter.Archive("lane"); err != nil {
		t.Fatal(err)
	}
}
