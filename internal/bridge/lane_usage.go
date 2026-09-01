package bridge

import "github.com/antst/agent-sessions/internal/sessiontools"

// LaneUsage returns the established native lane help for one supported product.
// Keeping this projection in the bridge preserves the pre-unification CLI
// contract while the unified binary owns dispatch and daemon communication.
func LaneUsage(product string) (string, bool) {
	usage, err := sessiontools.LaneUsage(product)
	return usage, err == nil
}
