package federator

import "github.com/antst/agent-sessions/internal/productcatalog"

const (
	// CapabilityCodexLane remains a compatibility projection of the shared inventory.
	CapabilityCodexLane = productcatalog.CapabilityCodexLane
	// CapabilityClaudeLane remains a compatibility projection of the shared inventory.
	CapabilityClaudeLane = productcatalog.CapabilityClaudeLane
	// CapabilityGrokLane remains a compatibility projection of the shared inventory.
	CapabilityGrokLane = productcatalog.CapabilityGrokLane
	// CapabilityQwenLane remains a compatibility projection of the shared inventory.
	CapabilityQwenLane = productcatalog.CapabilityQwenLane
)

// ProductDescriptor remains a compatibility alias. The descriptor table is
// owned by productcatalog.
type ProductDescriptor = productcatalog.ProductDescriptor

// ProductDescriptors returns the shared product inventory.
func ProductDescriptors() []ProductDescriptor { return productcatalog.ProductDescriptors() }

// ProductByID resolves a product through the shared inventory.
func ProductByID(id string) (ProductDescriptor, bool) { return productcatalog.ProductByID(id) }

// ProductByCapability resolves a lane capability through the shared inventory.
func ProductByCapability(capability string) (ProductDescriptor, bool) {
	return productcatalog.ProductByCapability(capability)
}
