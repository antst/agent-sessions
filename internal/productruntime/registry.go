package productruntime

import (
	"fmt"
	"reflect"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

// Registry is an immutable explicit runtime composition. Construction is the
// only registration path; package initialization never mutates it.
type Registry struct {
	ordered []RuntimeProduct
	byID    map[string]RuntimeProduct
}

// NewRegistry validates runtime products bidirectionally against an injected
// data-only catalog inventory.
func NewRegistry(inventory []productcatalog.Descriptor, products []RuntimeProduct) (*Registry, error) {
	if err := productcatalog.ValidateInventory(inventory); err != nil {
		return nil, fmt.Errorf("runtime inventory: %w", err)
	}
	wanted := make(map[string]productcatalog.Descriptor, len(inventory))
	order := make([]string, 0, len(inventory))
	for _, descriptor := range inventory {
		wanted[descriptor.ID] = cloneDescriptor(descriptor)
		order = append(order, descriptor.ID)
	}
	composed := make(map[string]RuntimeProduct, len(products))
	for _, product := range products {
		id := product.Descriptor.ID
		descriptor, ok := wanted[id]
		if !ok {
			return nil, fmt.Errorf("runtime product %q is absent from injected inventory", id)
		}
		if _, duplicate := composed[id]; duplicate {
			return nil, fmt.Errorf("duplicate runtime product %q", id)
		}
		if !reflect.DeepEqual(descriptor, product.Descriptor) {
			return nil, fmt.Errorf("runtime product %q descriptor differs from injected inventory", id)
		}
		if err := validateRuntimeProduct(product); err != nil {
			return nil, fmt.Errorf("runtime product %q: %w", id, err)
		}
		product.Descriptor = cloneDescriptor(product.Descriptor)
		composed[id] = product
	}
	if len(composed) != len(wanted) {
		for _, id := range order {
			if _, ok := composed[id]; !ok {
				return nil, fmt.Errorf("catalog product %q has no runtime composition", id)
			}
		}
	}
	registry := &Registry{ordered: make([]RuntimeProduct, 0, len(order)), byID: make(map[string]RuntimeProduct, len(order))}
	for _, id := range order {
		product := composed[id]
		registry.ordered = append(registry.ordered, product)
		registry.byID[id] = product
	}
	return registry, nil
}

func validateRuntimeProduct(product RuntimeProduct) error {
	descriptor := product.Descriptor
	interactive := descriptor.Has(productcatalog.CapabilityInteractive)
	lane := descriptor.Has(productcatalog.CapabilityLane)
	parent := descriptor.Has(productcatalog.CapabilityParent)
	componentTransport := interactive && descriptor.PeerTransport == ComponentPeerTransport
	if interactive != (product.Peer != nil) {
		return fmt.Errorf("interactive capability/peer driver mismatch")
	}
	if interactive != (product.Message != nil) {
		return fmt.Errorf("interactive capability/message driver mismatch")
	}
	if lane != (product.Lane != nil) {
		return fmt.Errorf("lane capability/driver mismatch")
	}
	if parent != (product.Parent != nil) {
		return fmt.Errorf("parent capability/attester mismatch")
	}
	if descriptor.SupportState != productcatalog.SupportHidden && product.Doctor == nil {
		return fmt.Errorf("visible product requires doctor probe")
	}
	if !componentTransport && (product.ComponentResolver != nil || product.ComponentRebinder != nil) {
		return fmt.Errorf("component resolver/rebinder requires interactive component peer transport")
	}
	if product.ComponentRebinder != nil && product.ComponentResolver == nil {
		return fmt.Errorf("component rebinder requires component resolver")
	}
	return nil
}

func (r *Registry) ByID(id string) (RuntimeProduct, bool) {
	if r == nil {
		return RuntimeProduct{}, false
	}
	product, ok := r.byID[id]
	if !ok {
		return RuntimeProduct{}, false
	}
	return cloneRuntimeProduct(product), true
}

func (r *Registry) All() []RuntimeProduct {
	if r == nil {
		return nil
	}
	result := make([]RuntimeProduct, len(r.ordered))
	for index, product := range r.ordered {
		result[index] = cloneRuntimeProduct(product)
	}
	return result
}

func cloneRuntimeProduct(product RuntimeProduct) RuntimeProduct {
	product.Descriptor = cloneDescriptor(product.Descriptor)
	return product
}

func cloneDescriptor(descriptor productcatalog.Descriptor) productcatalog.Descriptor {
	descriptor.PluginArchivePaths = append([]string(nil), descriptor.PluginArchivePaths...)
	descriptor.Capabilities = append([]productcatalog.Capability(nil), descriptor.Capabilities...)
	descriptor.Compatibility.TupleMembers = append([]productcatalog.TupleMember(nil), descriptor.Compatibility.TupleMembers...)
	descriptor.RequiredDoctorFeatures = append([]string(nil), descriptor.RequiredDoctorFeatures...)
	descriptor.FederationCapabilities = append([]string(nil), descriptor.FederationCapabilities...)
	return descriptor
}
