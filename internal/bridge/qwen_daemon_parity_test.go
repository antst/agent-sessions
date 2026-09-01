package bridge

import "testing"

// TestQwenDaemonParity runs every exact working-baseline Qwen assertion before
// any daemon adapter or shared attachment mechanism is extracted.
func TestQwenDaemonParity(t *testing.T) {
	runMappedLegacyParity(t, "Q-LAUNCH", "Q-HOST")
}
