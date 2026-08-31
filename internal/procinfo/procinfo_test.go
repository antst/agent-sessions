package procinfo

import "testing"

func TestLooksLikeCodexHost(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"/usr/local/bin/codex"}, want: true},
		{args: []string{"/usr/bin/node", "/opt/codex/codex.js"}, want: true},
		{args: []string{"/usr/bin/node-22", "/opt/codex/codex.js"}, want: true},
		{args: []string{"/usr/bin/node", "/opt/other.js"}, want: false},
		{args: []string{"/bin/sh", "codex"}, want: false},
		{args: nil, want: false},
	}
	for _, test := range tests {
		if got := LooksLikeCodexHost(test.args); got != test.want {
			t.Fatalf("LooksLikeCodexHost(%q) = %v, want %v", test.args, got, test.want)
		}
	}
}

func TestObservableEnvironmentRejectsMissingIdentityEvidence(t *testing.T) {
	if _, err := observableEnvironment(42, nil); err == nil {
		t.Fatal("empty process environment was accepted as authoritative")
	}
	want := []string{"AGENT_SESSIONS_PRODUCT=qwen"}
	got, err := observableEnvironment(42, want)
	if err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("observable process environment = %v, %v", got, err)
	}
}
