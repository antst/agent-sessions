package permissionmode

import "testing"

func TestFromArgsPreservesBoundariesAndPrecedence(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "default", args: []string{"claude"}, want: "default"},
		{name: "dangerous", args: []string{"claude", "--dangerously-skip-permissions"}, want: "bypassPermissions"},
		{name: "dangerous disabled", args: []string{"claude", "--dangerously-skip-permissions", "--dangerously-skip-permissions=false"}, want: "default"},
		{name: "dangerous overrides later mode", args: []string{"claude", "--dangerously-skip-permissions", "--permission-mode", "plan"}, want: "bypassPermissions"},
		{name: "last ordinary mode", args: []string{"grok", "--always-approve", "--permission-mode", "default"}, want: "default"},
		{name: "codex compact", args: []string{"codex", "-anever"}, want: "bypassPermissions"},
		{name: "prompt boundary", args: []string{"claude", "--", "--dangerously-skip-permissions"}, want: "default"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := FromArgs(test.args); got != test.want {
				t.Fatalf("FromArgs(%q) = %q, want %q", test.args, got, test.want)
			}
		})
	}
}

func TestTypedModeParsesWithoutChangingLegacyFromArgs(t *testing.T) {
	for _, want := range []Mode{Default, BypassPermissions} {
		got, err := Parse(string(want))
		if err != nil || got != want || !got.Valid() {
			t.Fatalf("Parse(%q) = %q, %v", want, got, err)
		}
	}
	for _, value := range []string{"", "yolo", "bypass", "unknown"} {
		if got, err := Parse(value); err == nil || got.Valid() {
			t.Fatalf("Parse(%q) = %q, %v; want invalid", value, got, err)
		}
	}
	if got := FromArgs([]string{"grok", "--always-approve"}); got != string(BypassPermissions) {
		t.Fatalf("legacy FromArgs = %q", got)
	}
}
