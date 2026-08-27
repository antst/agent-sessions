package bridge

import "github.com/antst/agent-sessions/internal/productcatalog"

type bridgeProduct struct {
	descriptor productcatalog.ProductDescriptor
}

func bridgeProductByID(product string) (bridgeProduct, bool) {
	descriptor, ok := productcatalog.ProductByID(product)
	return bridgeProduct{descriptor: descriptor}, ok
}

func bridgeProductByLaneRole(role string) (bridgeProduct, bool) {
	for _, descriptor := range productcatalog.ProductDescriptors() {
		if descriptor.LaneRuntimeRole == role {
			return bridgeProduct{descriptor: descriptor}, true
		}
	}
	return bridgeProduct{}, false
}

func mcpLaneProductIDs() []string {
	descriptors := productcatalog.ProductDescriptors()
	products := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		products = append(products, descriptor.ID)
	}
	return products
}
