package main

import (
	"context"
	"os"
	"testing"

	"github.com/antst/agent-sessions/wrappers/host"
)

func TestUnavailableEntries(t *testing.T) {
	t.Setenv(host.TokenEnv, "")
	_ = os.Unsetenv(host.TokenEnv)
	for _, test := range []struct {
		arguments []string
		want      string
	}{
		{[]string{"mcp"}, "mcp entry not available in this build"},
		{nil, "interactive entry not available in this build"},
	} {
		err := run(context.Background(), test.arguments)
		if err == nil || err.Error() != test.want {
			t.Fatalf("run(%q) = %v", test.arguments, err)
		}
	}
}
