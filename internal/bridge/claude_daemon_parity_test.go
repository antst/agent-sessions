package bridge

import "testing"

// TestClaudeDaemonParity runs every exact working-baseline Claude assertion
// before any daemon adapter or shared attachment mechanism is extracted.
func TestClaudeDaemonParity(t *testing.T) {
	runMappedLegacyParity(t, "CL-LAUNCH", "CL-MCP")
}
