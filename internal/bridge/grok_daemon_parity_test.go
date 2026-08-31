package bridge

import "testing"

// TestGrokDaemonParity runs every exact working-baseline Grok assertion before
// any daemon adapter or shared attachment mechanism is extracted.
func TestGrokDaemonParity(t *testing.T) {
	runMappedLegacyParity(t, "G-LAUNCH", "G-HOST")
}
