//go:build darwin

package pathidentity

import (
	"path/filepath"
	"testing"
)

func TestFuturePathCanonicalizesDarwinSystemAliases(t *testing.T) {
	for _, test := range []struct{ alias, target string }{
		{alias: "/tmp", target: "/private/tmp"},
		{alias: "/var", target: "/private/var"},
	} {
		input := filepath.Join(test.alias, "agent-sessions-path-identity-test", "profile")
		got, err := FuturePath(input)
		want := filepath.Join(test.target, "agent-sessions-path-identity-test", "profile")
		if err != nil || got != want {
			t.Fatalf("FuturePath(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}
