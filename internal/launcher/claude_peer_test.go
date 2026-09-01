package launcher

import (
	"reflect"
	"testing"
)

func TestClaudePeerPassesProductOwnedNameAndResumeSelectors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "fresh named",
			args: []string{"--peer-name", "reviewer", "--model", "sonnet"},
			want: []string{"--model", "sonnet", "--name", "reviewer"},
		},
		{
			name: "resume by name",
			args: []string{"--resume", "reviewer"},
			want: []string{"--resume", "reviewer"},
		},
		{
			name: "resume by id",
			args: []string{"--resume", "00000000-0000-4000-8000-000000000123"},
			want: []string{"--resume", "00000000-0000-4000-8000-000000000123"},
		},
		{
			name: "exact session id",
			args: []string{"--session-id", "00000000-0000-4000-8000-000000000124"},
			want: []string{"--session-id", "00000000-0000-4000-8000-000000000124"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := parseClaudePeerArgs(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plan.args, test.want) {
				t.Fatalf("native argv = %#v, want %#v", plan.args, test.want)
			}
		})
	}
}

func TestClaudePeerRemovesOnlyWrapperContext(t *testing.T) {
	plan, err := parseClaudePeerArgs([]string{
		"--group", "project", "--group=review", "--yolo", "--", "prompt", "--group", "literal",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--dangerously-skip-permissions", "--", "prompt", "--group", "literal"}
	if !reflect.DeepEqual(plan.args, want) {
		t.Fatalf("native argv = %#v, want %#v", plan.args, want)
	}
	if !plan.context.groupsSpecified || !reflect.DeepEqual(plan.context.groups, []string{"project", "review"}) {
		t.Fatalf("wrapper groups = %#v", plan.context.groups)
	}
}

func TestClaudePeerNoYoloIsNativeLastDecision(t *testing.T) {
	plan, err := parseClaudePeerArgs([]string{"--dangerously-skip-permissions", "--no-yolo", "--", "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--dangerously-skip-permissions", "--permission-mode", "default", "--", "prompt",
	}
	if !reflect.DeepEqual(plan.args, want) {
		t.Fatalf("native argv = %#v, want %#v", plan.args, want)
	}
}

func TestClaudePeerNativeOptionValueIsNotParsedAsWrapperFlag(t *testing.T) {
	plan, err := parseClaudePeerArgs([]string{"--model", "--group", "--peer-name", "worker"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--model", "--group", "--name", "worker"}
	if !reflect.DeepEqual(plan.args, want) {
		t.Fatalf("native argv = %#v, want %#v", plan.args, want)
	}
}
