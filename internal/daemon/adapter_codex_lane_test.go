package daemon

import (
	"context"
	"reflect"
	"testing"
)

func TestCodexLaneAdapterPreservesAppServerStartResumePolicyAndNativeCallbacks(t *testing.T) {
	var starts, resumes []CodexLaneRequest
	var prompts []CodexLanePrompt
	interrupted, archived := false, false
	adapter, err := NewCodexLaneAdapter(CodexLaneAdapterConfig{
		Start: func(_ context.Context, request CodexLaneRequest) (CodexLaneSession, error) {
			starts = append(starts, request)
			return CodexLaneSession{ID: request.LaneID}, nil
		},
		Resume: func(_ context.Context, request CodexLaneRequest) (CodexLaneSession, error) {
			resumes = append(resumes, request)
			return CodexLaneSession{ID: request.NativeSession}, nil
		},
		StartTurn: func(_ context.Context, prompt CodexLanePrompt) (string, error) {
			prompts = append(prompts, prompt)
			return "native-turn", nil
		},
		Wait: func(_ context.Context, threadID, turnID string) (CodexLaneTerminal, error) {
			return CodexLaneTerminal{ThreadID: threadID, TurnID: turnID, Outcome: "completed", Result: "done"}, nil
		},
		Interrupt: func(context.Context, string, string) error { interrupted = true; return nil },
		Archive:   func(context.Context, string) error { archived = true; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := adapter.Prepare(context.Background(), CodexLaneRequest{
		LaneID: "lane", Cwd: "/workspace", Name: "worker", PermissionMode: "bypassPermissions",
	})
	if err != nil || started.ID != "lane" || started.ApprovalPolicy != "never" || started.Sandbox != "danger-full-access" ||
		len(starts) != 1 || starts[0].ApprovalPolicy != "never" || starts[0].Sandbox != "danger-full-access" {
		t.Fatalf("start = %+v, starts=%+v, %v", started, starts, err)
	}
	resumed, err := adapter.Prepare(context.Background(), CodexLaneRequest{
		LaneID: "lane", NativeSession: "native", Cwd: "/workspace", Resume: true, Unarchive: true,
		ApprovalPolicy: "on-request", Sandbox: "workspace-write",
	})
	if err != nil || resumed.ID != "native" || len(resumes) != 1 || !resumes[0].Unarchive {
		t.Fatalf("resume = %+v, resumes=%+v, %v", resumed, resumes, err)
	}
	turnID, err := adapter.StartTurn(context.Background(), CodexLanePrompt{
		ThreadID: "native", Prompt: "work", Effort: "high", Arguments: []string{"--model", "x"},
	})
	if err != nil || turnID != "native-turn" || len(prompts) != 1 || !reflect.DeepEqual(prompts[0].Arguments, []string{"--model", "x"}) {
		t.Fatalf("turn = %q, prompts=%+v, %v", turnID, prompts, err)
	}
	if result, err := adapter.Wait(context.Background(), "native", turnID); err != nil || result.Result != "done" {
		t.Fatalf("wait = %+v, %v", result, err)
	}
	if err := adapter.Interrupt(context.Background(), "native", turnID); err != nil || !interrupted {
		t.Fatalf("interrupt = %v, called=%v", err, interrupted)
	}
	if err := adapter.Archive(context.Background(), "native"); err != nil || !archived {
		t.Fatalf("archive = %v, called=%v", err, archived)
	}
}

func TestCodexLaneAdapterRejectsChangedResumeAndTerminalIdentity(t *testing.T) {
	adapter, err := NewCodexLaneAdapter(CodexLaneAdapterConfig{
		Start: func(_ context.Context, request CodexLaneRequest) (CodexLaneSession, error) {
			return CodexLaneSession{ID: request.LaneID}, nil
		},
		Resume: func(context.Context, CodexLaneRequest) (CodexLaneSession, error) {
			return CodexLaneSession{ID: "other"}, nil
		},
		StartTurn: func(context.Context, CodexLanePrompt) (string, error) { return "turn", nil },
		Wait: func(context.Context, string, string) (CodexLaneTerminal, error) {
			return CodexLaneTerminal{ThreadID: "other", TurnID: "turn"}, nil
		},
		Interrupt: func(context.Context, string, string) error { return nil },
		Archive:   func(context.Context, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Prepare(context.Background(), CodexLaneRequest{LaneID: "lane", NativeSession: "native", Cwd: "/workspace", Resume: true}); err == nil {
		t.Fatal("changed resumed thread was accepted")
	}
	if _, err := adapter.Wait(context.Background(), "native", "turn"); err == nil {
		t.Fatal("changed terminal thread was accepted")
	}
}
