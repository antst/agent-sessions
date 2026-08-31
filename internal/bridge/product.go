package bridge

import "github.com/antst/agent-sessions/internal/federator"

type bridgeProduct struct {
	descriptor federator.ProductDescriptor
}

func bridgeProductByID(product string) (bridgeProduct, bool) {
	descriptor, ok := federator.ProductByID(product)
	return bridgeProduct{descriptor: descriptor}, ok
}

func bridgeProductByLaneRole(role string) (bridgeProduct, bool) {
	for _, descriptor := range federator.ProductDescriptors() {
		if descriptor.LaneRuntimeRole == role {
			return bridgeProduct{descriptor: descriptor}, true
		}
	}
	return bridgeProduct{}, false
}

func mcpLaneProductIDs() []string {
	descriptors := federator.ProductDescriptors()
	products := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		products = append(products, descriptor.ID)
	}
	return products
}
