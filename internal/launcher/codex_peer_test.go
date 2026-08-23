package launcher

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestCodexPeerNativeArgumentParity(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		mode codexMode
		want []string
	}{
		{name: "fresh", args: []string{"-n", "reviewer", "--no-alt-screen"}, mode: modeFresh, want: []string{"--no-alt-screen"}},
		{name: "resume", args: []string{"resume", "reviewer", "--yolo"}, mode: modeResume, want: []string{"--yolo"}},
		{name: "global yolo resume", args: []string{"--yolo", "resume", "reviewer", "--no-alt-screen"}, mode: modeResume, want: []string{"--yolo", "--no-alt-screen"}},
		{name: "global display resume", args: []string{"--no-alt-screen", "resume", "reviewer"}, mode: modeResume, want: []string{"--no-alt-screen"}},
		{name: "prompt boundary", args: []string{"-C", realDirectory, "--", "-C", "prompt-data"}, mode: modeFresh, want: []string{"-C", realDirectory, "--", "-C", "prompt-data"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := parseCodexPeerArgs(test.args, root, "")
			if err != nil {
				t.Fatal(err)
			}
			if plan.mode != test.mode || !reflect.DeepEqual(plan.interactiveArgs, test.want) {
				t.Fatalf("plan = %+v, want mode=%s args=%q", plan, test.mode, test.want)
			}
			if test.mode == modeResume && plan.selectionTarget != "reviewer" {
				t.Fatalf("selection target = %q", plan.selectionTarget)
			}
			if test.mode == modeResume && slices.Contains(test.args, "--yolo") && !plan.requestedYolo {
				t.Fatalf("resume yolo was not mirrored into the prepared binding: %+v", plan)
			}
		})
	}
}

func TestResumedBindingMirrorsYoloInEitherInputPosition(t *testing.T) {
	root := t.TempDir()
	for _, input := range [][]string{
		{"resume", "reviewer", "--yolo"},
		{"--yolo", "resume", "reviewer"},
	} {
		plan, err := parseCodexPeerArgs(input, root, "")
		if err != nil {
			t.Fatal(err)
		}
		got := resumedBindArguments(plan, 42, "start-token")
		wantSuffix := []string{"--approval-policy", "never", "--sandbox", "danger-full-access"}
		if len(got) < len(wantSuffix) || !reflect.DeepEqual(got[len(got)-len(wantSuffix):], wantSuffix) {
			t.Fatalf("bind args for %q = %q", input, got)
		}
	}
}

func TestCodexPeerInformationalAndPassthroughPreserveNativeArgv(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"-n", "ignored", "--version"},
		{"-C", root, "completion", "bash"},
		{"app"},
		{"agents"},
		{"queue", "thread", "follow-up"},
		{"migrate-rollouts"},
	} {
		plan, err := parseCodexPeerArgs(args, root, "")
		if err != nil {
			t.Fatal(err)
		}
		if !plan.informationalPass && plan.mode != modePassthrough {
			t.Fatalf("not passthrough: %+v", plan)
		}
	}
}

func TestCodexPeerForwardsNativeOptionsAndRejectsUnattestedOperations(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		args []string
		want []string
	}{
		{args: []string{"--model", "gpt-5", "prompt"}, want: []string{"--model", "gpt-5", "prompt"}},
		{args: []string{"--profile=testing", "--search", "resume", "thread", "--sandbox", "read-only"}, want: []string{"--profile=testing", "--search", "--sandbox", "read-only"}},
		{args: []string{"resume", "--all", "--model", "gpt-5", "thread", "--dangerously-bypass-hook-trust"}, want: []string{"--all", "--model", "gpt-5", "--dangerously-bypass-hook-trust"}},
		{args: []string{"-mresume", "resume", "thread", "-a", "never"}, want: []string{"-mresume", "-a", "never"}},
	} {
		plan, err := parseCodexPeerArgs(test.args, root, "")
		if err != nil {
			t.Fatalf("parse %q: %v", test.args, err)
		}
		if !reflect.DeepEqual(plan.interactiveArgs, test.want) {
			t.Fatalf("forwarded argv changed: got %q want %q", plan.interactiveArgs, test.want)
		}
	}
	for _, args := range [][]string{
		{"resume", "--last"},
		{"resume", "thread", "--remote", "tcp://host"},
		{"resume", "thread", "--remote-auth-token-env=TOKEN"},
	} {
		if _, err := parseCodexPeerArgs(args, root, ""); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}
