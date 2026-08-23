package launcher

import "github.com/antst/agent-sessions/internal/federator"

type launcherProduct struct {
	descriptor federator.ProductDescriptor
}

func launcherProductByID(product string) (launcherProduct, bool) {
	descriptor, ok := federator.ProductByID(product)
	return launcherProduct{descriptor: descriptor}, ok
}

func (product launcherProduct) resume(kind, sessionID string) (string, []string, bool) {
	arguments, ok := product.descriptor.ResumeArguments(kind, sessionID)
	if !ok {
		return "", nil, false
	}
	executable := product.descriptor.PeerExecutable
	if kind == federator.SessionKindLane {
		executable = product.descriptor.LaneExecutable
	}
	return executable, arguments, true
}
