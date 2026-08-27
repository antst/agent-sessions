package bridge

import (
	"fmt"
	"io"
	"os"

	daemoncore "github.com/antst/agent-sessions/internal/daemon"
)

func runGrokPluginVerify(args []string) int {
	if len(args) != 2 || args[0] != "--root" || args[1] == "" {
		fmt.Fprintln(os.Stderr, "usage: agent-session-runtime grok-plugin-verify --root <user-plugin-root>")
		return 2
	}
	if err := verifyGrokPluginInspection(os.Stdin, args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime grok-plugin-verify: %v\n", err)
		return 1
	}
	return 0
}

// verifyGrokPluginInspection is retained as a compatibility boundary for the
// retiring runtime command. The canonical connector transaction and this
// verifier now share one exact daemon-owned inventory implementation.
func verifyGrokPluginInspection(reader io.Reader, expectedRoot string) error {
	body, err := io.ReadAll(io.LimitReader(reader, 2*1024*1024+1))
	if err != nil {
		return fmt.Errorf("read grok inspect --json: %w", err)
	}
	if len(body) > 2*1024*1024 {
		return fmt.Errorf("grok inspect --json exceeds 2 MiB")
	}
	return daemoncore.VerifyGrokConnectorInventory(body, expectedRoot)
}
