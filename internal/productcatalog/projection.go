package productcatalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

const ProjectionSchemaV1 = "agent-sessions.product-catalog.v1"

// Projection is the deterministic, secret-free staged-binary product view.
type Projection struct {
	Schema   string             `json:"schema"`
	Products []ProjectedProduct `json:"products"`
}

type ProjectedProduct struct {
	ID                     string             `json:"id"`
	Label                  string             `json:"label"`
	SupportState           SupportState       `json:"support_state"`
	NativeExecutable       string             `json:"native_executable"`
	TestedVersion          string             `json:"tested_version"`
	Compatibility          Compatibility      `json:"compatibility"`
	PeerAlias              string             `json:"peer_alias"`
	LaneAlias              string             `json:"lane_alias"`
	Capabilities           []string           `json:"capabilities"`
	FederationCapabilities []string           `json:"federation_capabilities"`
	PeerTransport          string             `json:"peer_transport"`
	MessageTransport       string             `json:"message_transport"`
	LaneTransport          string             `json:"lane_transport"`
	DoctorProbeKey         string             `json:"doctor_probe"`
	PermissionProfileKey   string             `json:"permission_profile"`
	InstallRoot            string             `json:"install_root"`
	PluginArchivePaths     []string           `json:"plugin_archive_paths"`
	RequiredDoctorFeatures []string           `json:"required_doctor_features"`
	NativeRegistration     NativeRegistration `json:"native_registration"`
	Acceptance             AcceptanceContract `json:"acceptance"`
}

// BuildProjection validates and deterministically sorts an injected inventory.
func BuildProjection(inventory []Descriptor) (Projection, error) {
	if err := ValidateInventory(inventory); err != nil {
		return Projection{}, err
	}
	products := make([]ProjectedProduct, 0, len(inventory))
	for _, descriptor := range inventory {
		federation := append([]string(nil), descriptor.FederationCapabilities...)
		doctorFeatures := append([]string(nil), descriptor.RequiredDoctorFeatures...)
		archivePaths := append([]string(nil), descriptor.PluginArchivePaths...)
		tuple := append([]TupleMember(nil), descriptor.Compatibility.TupleMembers...)
		registrationArgs := append([]string(nil), descriptor.NativeRegistration.Args...)
		externalCells := append([]ExternalAcceptanceCell(nil), descriptor.Acceptance.ExternalCells...)
		sort.Strings(federation)
		sort.Strings(doctorFeatures)
		sort.Strings(archivePaths)
		sort.Slice(tuple, func(i, j int) bool {
			if tuple[i].Name == tuple[j].Name {
				return tuple[i].Version < tuple[j].Version
			}
			return tuple[i].Name < tuple[j].Name
		})
		sort.Strings(registrationArgs)
		sort.Slice(externalCells, func(i, j int) bool { return externalCells[i].ID < externalCells[j].ID })
		products = append(products, ProjectedProduct{
			ID: descriptor.ID, Label: descriptor.Label, SupportState: descriptor.SupportState,
			NativeExecutable: descriptor.NativeExecutable, TestedVersion: descriptor.TestedVersion,
			Compatibility: Compatibility{Policy: descriptor.Compatibility.Policy, PackageManager: descriptor.Compatibility.PackageManager, PackageManagerVersion: descriptor.Compatibility.PackageManagerVersion, TupleMembers: tuple},
			PeerAlias:     descriptor.PeerAlias, LaneAlias: descriptor.LaneAlias,
			Capabilities: descriptor.SortedCapabilities(), FederationCapabilities: federation,
			PeerTransport: descriptor.PeerTransport, MessageTransport: descriptor.MessageTransport,
			LaneTransport:  descriptor.LaneTransport,
			DoctorProbeKey: descriptor.DoctorProbeKey, PermissionProfileKey: descriptor.PermissionProfileKey,
			InstallRoot: descriptor.InstallRoot, PluginArchivePaths: archivePaths, RequiredDoctorFeatures: doctorFeatures,
			NativeRegistration: NativeRegistration{Strategy: descriptor.NativeRegistration.Strategy, Args: registrationArgs, AssetOnly: descriptor.NativeRegistration.AssetOnly},
			Acceptance:         AcceptanceContract{RealProductRequired: descriptor.Acceptance.RealProductRequired, ExternalCells: externalCells},
		})
	}
	sort.Slice(products, func(i, j int) bool { return products[i].ID < products[j].ID })
	return Projection{Schema: ProjectionSchemaV1, Products: products}, nil
}

// ProjectionJSON renders canonical indented JSON with exactly one trailing
// newline. It performs no filesystem, environment, daemon, or native discovery.
func ProjectionJSON(inventory []Descriptor) ([]byte, error) {
	projection, err := BuildProjection(inventory)
	if err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(projection, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode product catalog projection: %w", err)
	}
	return append(bytes.TrimRight(body, "\n"), '\n'), nil
}
