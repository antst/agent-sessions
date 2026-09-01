package bridge

import "testing"

// TestCodexDaemonParity runs every exact working-baseline Codex assertion
// before any daemon adapter or shared attachment mechanism is extracted.
func TestCodexDaemonParity(t *testing.T) {
	runMappedLegacyParity(t, "C-LAUNCH", "C-SUP")
}
