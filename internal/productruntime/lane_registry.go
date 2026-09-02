package productruntime

import (
	"fmt"
	"strings"
)

// LaneRegistry is the immutable product-neutral dispatch table used by the
// host lane engine. Products enter it only at the explicit composition root.
type LaneRegistry struct {
	byProduct map[string]LaneDriver
}

func NewLaneRegistry(drivers map[string]LaneDriver) (*LaneRegistry, error) {
	registry := &LaneRegistry{byProduct: make(map[string]LaneDriver, len(drivers))}
	for product, driver := range drivers {
		if strings.TrimSpace(product) == "" || driver == nil {
			return nil, fmt.Errorf("lane registry contains an incomplete product driver")
		}
		registry.byProduct[product] = driver
	}
	return registry, nil
}

func (registry *LaneRegistry) ByProduct(product string) (LaneDriver, bool) {
	if registry == nil {
		return nil, false
	}
	driver, ok := registry.byProduct[product]
	return driver, ok
}
