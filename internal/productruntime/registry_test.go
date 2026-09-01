package productruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestRegistrySupportsInjectedSyntheticProductWithoutInitRegistration(t *testing.T) {
	descriptor := syntheticDescriptor("synthetic")
	product := completeRuntime(descriptor)
	registry, err := NewRegistry([]productcatalog.Descriptor{descriptor}, []RuntimeProduct{product})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := registry.ByID("synthetic")
	if !ok || got.Descriptor.ID != "synthetic" || len(registry.All()) != 1 {
		t.Fatalf("synthetic runtime = %#v, %v", got, ok)
	}
	got.Descriptor.Capabilities[0] = "mutated"
	again, _ := registry.ByID("synthetic")
	if again.Descriptor.Capabilities[0] == "mutated" {
		t.Fatal("registry leaked descriptor mutation")
	}
	if _, present := productcatalog.ByID("synthetic"); present {
		t.Fatal("synthetic runtime escaped into production catalog")
	}
}

func TestRegistryAllowsTruthfulUnsupportedRenameDriver(t *testing.T) {
	descriptor := syntheticDescriptor("synthetic")
	product := completeRuntime(descriptor)
	product.Peer = fakeUnsupportedRenamePeerDriver{}
	registry, err := NewRegistry([]productcatalog.Descriptor{descriptor}, []RuntimeProduct{product})
	if err != nil {
		t.Fatalf("compose rename-unsupported product: %v", err)
	}
	runtimeProduct, ok := registry.ByID(descriptor.ID)
	if !ok {
		t.Fatal("rename-unsupported product missing from registry")
	}
	_, err = runtimeProduct.Peer.Rename(context.Background(), daemon.ManagedAttachment{}, "new name")
	if !errors.Is(err, ErrUnsupportedRename) {
		t.Fatalf("rename unsupported category = %v", err)
	}
}

func TestRegistryScopesOptionalNativeTitleProjectorToInteractiveProducts(t *testing.T) {
	descriptor := syntheticDescriptor("synthetic")
	product := completeRuntime(descriptor)
	product.NativeTitle = fakeNativeTitleProjector{}
	registry, err := NewRegistry([]productcatalog.Descriptor{descriptor}, []RuntimeProduct{product})
	if err != nil {
		t.Fatalf("compose optional native title projector: %v", err)
	}
	got, ok := registry.ByID(descriptor.ID)
	if !ok || got.NativeTitle == nil {
		t.Fatalf("native title projector = %#v, %v", got.NativeTitle, ok)
	}
	projection, err := got.NativeTitle.ProjectNativeTitle(context.Background(), daemon.ManagedAttachment{})
	if err != nil || projection != (NativeTitleProjection{NativeSessionID: "native", Title: "title"}) {
		t.Fatalf("native title projection = %#v, %v", projection, err)
	}

	withoutProjector := completeRuntime(descriptor)
	if _, err := NewRegistry([]productcatalog.Descriptor{descriptor}, []RuntimeProduct{withoutProjector}); err != nil {
		t.Fatalf("optional native title projector became mandatory: %v", err)
	}

	nonInteractiveDescriptor := descriptor
	nonInteractiveDescriptor.Capabilities = []productcatalog.Capability{productcatalog.CapabilityLane, productcatalog.CapabilityParent}
	nonInteractive := completeRuntime(nonInteractiveDescriptor)
	nonInteractive.Peer = nil
	nonInteractive.Message = nil
	nonInteractive.NativeTitle = fakeNativeTitleProjector{}
	if _, err := NewRegistry([]productcatalog.Descriptor{nonInteractiveDescriptor}, []RuntimeProduct{nonInteractive}); err == nil {
		t.Fatal("native title projector accepted without interactive capability")
	}
}

func TestRegistryRejectsMissingExtraAndMismatchedDrivers(t *testing.T) {
	descriptor := syntheticDescriptor("synthetic")
	complete := completeRuntime(descriptor)
	tests := []struct {
		name      string
		inventory []productcatalog.Descriptor
		products  []RuntimeProduct
	}{
		{name: "missing runtime", inventory: []productcatalog.Descriptor{descriptor}},
		{name: "duplicate runtime", inventory: []productcatalog.Descriptor{descriptor}, products: []RuntimeProduct{complete, complete}},
		{name: "unknown runtime", inventory: []productcatalog.Descriptor{descriptor}, products: []RuntimeProduct{completeRuntime(syntheticDescriptor("other"))}},
		{name: "missing peer", inventory: []productcatalog.Descriptor{descriptor}, products: []RuntimeProduct{{Descriptor: descriptor, Message: fakeMessageDriver{}, Lane: fakeLaneDriver{}, Parent: fakeParentAttester{}, Doctor: fakeDoctor{}}}},
		{name: "missing message", inventory: []productcatalog.Descriptor{descriptor}, products: []RuntimeProduct{{Descriptor: descriptor, Peer: fakePeerDriver{}, Lane: fakeLaneDriver{}, Parent: fakeParentAttester{}, Doctor: fakeDoctor{}}}},
		{name: "missing lane", inventory: []productcatalog.Descriptor{descriptor}, products: []RuntimeProduct{{Descriptor: descriptor, Peer: fakePeerDriver{}, Message: fakeMessageDriver{}, Parent: fakeParentAttester{}, Doctor: fakeDoctor{}}}},
		{name: "missing parent", inventory: []productcatalog.Descriptor{descriptor}, products: []RuntimeProduct{{Descriptor: descriptor, Peer: fakePeerDriver{}, Message: fakeMessageDriver{}, Lane: fakeLaneDriver{}, Doctor: fakeDoctor{}}}},
		{name: "missing doctor", inventory: []productcatalog.Descriptor{descriptor}, products: []RuntimeProduct{{Descriptor: descriptor, Peer: fakePeerDriver{}, Message: fakeMessageDriver{}, Lane: fakeLaneDriver{}, Parent: fakeParentAttester{}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRegistry(test.inventory, test.products); err == nil {
				t.Fatal("invalid registry accepted")
			}
		})
	}

	noInteractive := descriptor
	noInteractive.Capabilities = []productcatalog.Capability{productcatalog.CapabilityLane, productcatalog.CapabilityParent}
	extra := completeRuntime(noInteractive)
	if _, err := NewRegistry([]productcatalog.Descriptor{noInteractive}, []RuntimeProduct{extra}); err == nil {
		t.Fatal("undeclared peer/message drivers accepted")
	}
}

func TestRegistryScopesOptionalComponentResolverAndRebinderToComponentTransport(t *testing.T) {
	resolver := ComponentResolverFunc(func(
		context.Context,
		HostDeps,
		daemon.ManagedAttachment,
		ComponentPeerEvidence,
	) (ComponentResolution, error) {
		return ComponentResolution{}, nil
	})
	rebinder := ComponentSessionRebinderFunc(func(
		context.Context,
		HostDeps,
		daemon.ManagedAttachment,
		string,
		string,
		[]byte,
	) (daemon.NativeEvidence, error) {
		return daemon.NativeEvidence{}, nil
	})

	componentDescriptor := syntheticDescriptor("component-product")
	componentDescriptor.PeerTransport = ComponentPeerTransport
	componentProduct := completeRuntime(componentDescriptor)
	componentProduct.ComponentResolver = resolver
	componentProduct.ComponentRebinder = rebinder
	registry, err := NewRegistry([]productcatalog.Descriptor{componentDescriptor}, []RuntimeProduct{componentProduct})
	if err != nil {
		t.Fatalf("compose component authority seam: %v", err)
	}
	got, ok := registry.ByID(componentDescriptor.ID)
	if !ok || got.ComponentResolver == nil || got.ComponentRebinder == nil {
		t.Fatalf("component authority seam = %#v, %v", got, ok)
	}

	withoutSeam := completeRuntime(componentDescriptor)
	if _, err := NewRegistry([]productcatalog.Descriptor{componentDescriptor}, []RuntimeProduct{withoutSeam}); err != nil {
		t.Fatalf("optional component seam rejected transitional composition: %v", err)
	}

	nonComponentDescriptor := syntheticDescriptor("non-component-product")
	nonComponent := completeRuntime(nonComponentDescriptor)
	nonComponent.ComponentResolver = resolver
	if _, err := NewRegistry([]productcatalog.Descriptor{nonComponentDescriptor}, []RuntimeProduct{nonComponent}); err == nil {
		t.Fatal("component resolver accepted for non-component transport")
	}

	nonInteractiveDescriptor := componentDescriptor
	nonInteractiveDescriptor.ID = "non-interactive-component"
	nonInteractiveDescriptor.NativeExecutable = nonInteractiveDescriptor.ID
	nonInteractiveDescriptor.PeerAlias = nonInteractiveDescriptor.ID + "-peer"
	nonInteractiveDescriptor.LaneAlias = nonInteractiveDescriptor.ID + "-lane"
	nonInteractiveDescriptor.LaneCapability = nonInteractiveDescriptor.ID + "-lane-capability"
	nonInteractiveDescriptor.FederationCapabilities = []string{nonInteractiveDescriptor.LaneCapability}
	nonInteractiveDescriptor.InstallRoot = "integrations/" + nonInteractiveDescriptor.ID
	nonInteractiveDescriptor.Capabilities = []productcatalog.Capability{productcatalog.CapabilityLane, productcatalog.CapabilityParent}
	nonInteractive := completeRuntime(nonInteractiveDescriptor)
	nonInteractive.Peer = nil
	nonInteractive.Message = nil
	nonInteractive.ComponentResolver = resolver
	if _, err := NewRegistry([]productcatalog.Descriptor{nonInteractiveDescriptor}, []RuntimeProduct{nonInteractive}); err == nil {
		t.Fatal("component resolver accepted without interactive capability")
	}

	partial := completeRuntime(componentDescriptor)
	partial.ComponentRebinder = rebinder
	if _, err := NewRegistry([]productcatalog.Descriptor{componentDescriptor}, []RuntimeProduct{partial}); err == nil {
		t.Fatal("component rebinder accepted without resolver")
	}
}

func TestRuntimeSecretsCannotSerializeOrAppearInFormatting(t *testing.T) {
	secret := NewSensitiveValue("raw-secret-value")
	if secret.String() != "[REDACTED]" || secret.GoString() != "[REDACTED]" || secret.Reveal() != "raw-secret-value" {
		t.Fatalf("sensitive value contract failed: %s", secret)
	}
	if _, err := json.Marshal(secret); err == nil {
		t.Fatal("sensitive value serialized")
	}
	if _, err := json.Marshal(NativeCommand{Path: "/bin/native", SensitiveEnv: []SensitiveEnvVar{{Name: "TOKEN", Value: secret}}}); err == nil {
		t.Fatal("native command serialized")
	}
	detail := NewRedactedString("raw-secret-value\n"+strings.Repeat("x", maxRedactedDetailBytes+20), secret)
	encoded, err := json.Marshal(detail)
	if err != nil || len(detail.String()) > maxRedactedDetailBytes || strings.Contains(string(encoded), "raw-secret-value") || strings.Contains(detail.String(), "\n") {
		t.Fatalf("redacted detail leaked or exceeded its bound: %q, %v", encoded, err)
	}
	automatic := NewRedactedString("request failed: Authorization: Bearer native-token password=hunter2")
	if strings.Contains(automatic.String(), "native-token") || strings.Contains(automatic.String(), "hunter2") {
		t.Fatalf("named native credential leaked: %q", automatic)
	}
	_, err = (fakeLaneDriver{}).Steer(context.Background(), NativeTurnRef{}, TurnStartRequest{})
	if !errors.Is(err, ErrUnsupportedSteer) {
		t.Fatal("typed unsupported steer was lost")
	}
	if reflect.TypeOf(NativeSessionRef{}).NumField() != 3 || reflect.TypeOf(NativeTurnRef{}).NumField() != 2 {
		t.Fatal("native references grew endpoint/token fields")
	}
}

func TestRegistryRejectsUnknownCapabilityInsteadOfDroppingRequiredDrivers(t *testing.T) {
	descriptor := syntheticDescriptor("synthetic")
	descriptor.Capabilities[0] = productcatalog.Capability("interactiv")
	product := completeRuntime(descriptor)
	product.Peer = nil
	product.Message = nil
	if _, err := NewRegistry([]productcatalog.Descriptor{descriptor}, []RuntimeProduct{product}); err == nil {
		t.Fatal("unknown capability bypassed runtime driver validation")
	}
}

func TestHookPointsUseTheBoundedSharedTokenGrammar(t *testing.T) {
	point, err := NewTestHookPoint("before-native-io")
	if err != nil || point.String() != "before-native-io" {
		t.Fatalf("valid hook point = %q, %v", point, err)
	}
	for _, invalid := range []string{"", "BeforeNativeIO", "before--native", strings.Repeat("x", 65)} {
		if _, err := NewTestHookPoint(invalid); err == nil {
			t.Fatalf("invalid hook point %q accepted", invalid)
		}
	}
}

func syntheticDescriptor(id string) productcatalog.Descriptor {
	descriptor := productcatalog.All()[0]
	descriptor.ID = id
	descriptor.Label = "Synthetic"
	descriptor.NativeExecutable = id
	descriptor.PeerAlias = id + "-peer"
	descriptor.LaneAlias = id + "-peer-lane"
	descriptor.LaneCapability = id + "-lane"
	descriptor.FederationCapabilities = []string{id + "-lane"}
	descriptor.PeerTransport = id + "-peer"
	descriptor.MessageTransport = id + "-message"
	descriptor.LaneTransport = id + "-lane"
	descriptor.ConnectorAttesterKey = id + "-parent"
	descriptor.DoctorProbeKey = id + "-doctor"
	descriptor.PermissionProfileKey = id + "-permission"
	descriptor.InstallRoot = "integrations/" + id
	return descriptor
}

func completeRuntime(descriptor productcatalog.Descriptor) RuntimeProduct {
	return RuntimeProduct{Descriptor: descriptor, Peer: fakePeerDriver{}, Message: fakeMessageDriver{}, Lane: fakeLaneDriver{}, Parent: fakeParentAttester{}, Doctor: fakeDoctor{}}
}
