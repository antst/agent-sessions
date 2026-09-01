package releaseinstall

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

const InstallProjectionSchemaV1 = "agent-sessions.release-install-projection.v1"

type InstallProjection struct {
	Schema   string                    `json:"schema"`
	Products []ProjectedInstallProduct `json:"products"`
}

type ProjectedInstallProduct struct {
	ProductID          string                            `json:"product_id"`
	SupportState       productcatalog.SupportState       `json:"support_state"`
	TestedVersion      string                            `json:"tested_version"`
	NativeRegistration productcatalog.NativeRegistration `json:"native_registration"`
	Acceptance         productcatalog.AcceptanceContract `json:"acceptance"`
	Authority          *productcatalog.AuthorityContract `json:"authority,omitempty"`
}

func BuildProjection(inventory []productcatalog.Descriptor) (InstallProjection, error) {
	if err := productcatalog.ValidateInventory(inventory); err != nil {
		return InstallProjection{}, err
	}
	products := make([]ProjectedInstallProduct, 0, len(inventory))
	for _, descriptor := range sortedDescriptors(inventory) {
		args := append([]string(nil), descriptor.NativeRegistration.Args...)
		cells := append([]productcatalog.ExternalAcceptanceCell(nil), descriptor.Acceptance.ExternalCells...)
		sort.Strings(args)
		sort.Slice(cells, func(i, j int) bool { return cells[i].ID < cells[j].ID })
		var authority *productcatalog.AuthorityContract
		if descriptor.Authority != nil {
			copyAuthority := *descriptor.Authority
			authority = &copyAuthority
		}
		products = append(products, ProjectedInstallProduct{
			ProductID: descriptor.ID, SupportState: descriptor.SupportState, TestedVersion: descriptor.TestedVersion,
			NativeRegistration: productcatalog.NativeRegistration{Strategy: descriptor.NativeRegistration.Strategy, Args: args, AssetOnly: descriptor.NativeRegistration.AssetOnly},
			Acceptance:         productcatalog.AcceptanceContract{RealProductRequired: descriptor.Acceptance.RealProductRequired, ExternalCells: cells},
			Authority:          authority,
		})
	}
	return InstallProjection{Schema: InstallProjectionSchemaV1, Products: products}, nil
}

func ProjectionJSON(inventory []productcatalog.Descriptor) ([]byte, error) {
	projection, err := BuildProjection(inventory)
	if err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(projection, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode release install projection: %w", err)
	}
	return append(bytes.TrimRight(body, "\n"), '\n'), nil
}
