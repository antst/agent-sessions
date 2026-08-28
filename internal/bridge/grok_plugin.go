package bridge

import (
	"fmt"
	"io"

	daemoncore "github.com/antst/agent-sessions/internal/daemon"
)

// verifyGrokPluginInspection keeps the bridge and daemon connector paths on one
// exact daemon-owned inventory implementation.
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
