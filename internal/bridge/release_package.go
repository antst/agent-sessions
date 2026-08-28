package bridge

import (
	"fmt"
	"os"

	"github.com/antst/agent-sessions/internal/releasepkg"
)

// RunReleasePackage executes the repository-internal deterministic packaging
// helper from the canonical host image. It is intentionally not part of the
// public host command catalog.
func RunReleasePackage(args []string) int {
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "agent-sessions release-package requires STAGE_ROOT PACKAGE_NAME ARCHIVE")
		return 2
	}
	if err := releasepkg.Create(args[0], args[1], args[2]); err != nil {
		fmt.Fprintf(os.Stderr, "agent-sessions release-package: %v\n", err)
		return 1
	}
	return 0
}
