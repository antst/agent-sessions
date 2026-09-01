package bridge

// LaneUsage returns the established native lane help for one supported product.
// Keeping this projection in the bridge preserves the pre-unification CLI
// contract while the unified binary owns dispatch and daemon communication.
func LaneUsage(product string) (string, bool) {
	switch product {
	case "codex":
		return laneUsage(), true
	case "claude":
		return claudeLaneUsage(), true
	case "grok":
		return grokLaneUsage(), true
	case "qwen":
		return qwenLaneUsage(), true
	default:
		return "", false
	}
}
