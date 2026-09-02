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
	Compatibility      productcatalog.Compatibility      `json:"compatibility"`
	NativeRegistration productcatalog.NativeRegistration `json:"native_registration"`
	Acceptance         productcatalog.AcceptanceContract `json:"acceptance"`
}

func BuildProjection(inventory []productcatalog.Descriptor) (InstallProjection, error) {
	if err := productcatalog.ValidateInventory(inventory); err != nil {
		return InstallProjection{}, err
	}
	products := make([]ProjectedInstallProduct, 0, len(inventory))
	for _, descriptor := range sortedDescriptors(inventory) {
		args := append([]string(nil), descriptor.NativeRegistration.Args...)
		cells := append([]productcatalog.ExternalAcceptanceCell(nil), descriptor.Acceptance.ExternalCells...)
		tuple := append([]productcatalog.TupleMember(nil), descriptor.Compatibility.TupleMembers...)
		sort.Strings(args)
		sort.Slice(cells, func(i, j int) bool { return cells[i].ID < cells[j].ID })
		sort.Slice(tuple, func(i, j int) bool {
			if tuple[i].Name == tuple[j].Name {
				return tuple[i].Version < tuple[j].Version
			}
			return tuple[i].Name < tuple[j].Name
		})
		products = append(products, ProjectedInstallProduct{
			ProductID: descriptor.ID, SupportState: descriptor.SupportState, TestedVersion: descriptor.TestedVersion,
			Compatibility: productcatalog.Compatibility{
				Policy: descriptor.Compatibility.Policy, PackageManager: descriptor.Compatibility.PackageManager,
				PackageManagerVersion: descriptor.Compatibility.PackageManagerVersion, TupleMembers: tuple,
			},
			NativeRegistration: productcatalog.NativeRegistration{Strategy: descriptor.NativeRegistration.Strategy, Args: args, AssetOnly: descriptor.NativeRegistration.AssetOnly},
			Acceptance:         productcatalog.AcceptanceContract{RealProductRequired: descriptor.Acceptance.RealProductRequired, ExternalCells: cells},
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
