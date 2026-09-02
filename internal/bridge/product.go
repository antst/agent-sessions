package bridge

import "github.com/antst/agent-sessions/internal/productcatalog"

type bridgeProduct struct {
	descriptor productcatalog.Descriptor
}

func bridgeProductByID(product string) (bridgeProduct, bool) {
	descriptor, ok := productcatalog.ByID(product)
	return bridgeProduct{descriptor: descriptor}, ok
}

func bridgeProductByLaneRole(role string) (bridgeProduct, bool) {
	for _, descriptor := range productcatalog.All() {
		if descriptor.LaneRuntimeRole == role {
			return bridgeProduct{descriptor: descriptor}, true
		}
	}
	return bridgeProduct{}, false
}

func mcpLaneProductIDs() []string {
	descriptors := productcatalog.All()
	products := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		products = append(products, descriptor.ID)
	}
	return products
}
