package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestTenProductCatalogAndDerivedConsumers(t *testing.T) {
	if err := validateDescriptors(authoredDescriptors); err != nil {
		t.Fatal(err)
	}
	catalog, err := projectionBytes("catalog", authoredDescriptors)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"release-inventory", "install-plan", "acceptance-matrix"} {
		body, viewErr := projectionBytes(view, authoredDescriptors)
		if viewErr != nil || len(body) == 0 {
			t.Fatalf("%s = %d bytes, %v", view, len(body), viewErr)
		}
	}
	release := deriveReleaseInventory(normalizedDescriptors(authoredDescriptors))
	if len(release.HostAliases) != 20 || len(release.PluginPayloads) != 10 || len(release.FederationCapabilities) != 10 {
		t.Fatalf("release projection counts = aliases:%d payloads:%d federation:%d", len(release.HostAliases), len(release.PluginPayloads), len(release.FederationCapabilities))
	}
	install := deriveInstallPlan(normalizedDescriptors(authoredDescriptors))
	if len(install.Actions) != 10 {
		t.Fatalf("install actions = %d", len(install.Actions))
	}
	for _, action := range install.Actions {
		if !action.RollbackReceipt || len(action.AdvertiseWhenReady) != 1 {
			t.Fatalf("incomplete install action %#v", action)
		}
	}
	matrix := deriveAcceptanceMatrix(normalizedDescriptors(authoredDescriptors))
	if len(matrix.Cells) < 60 {
		t.Fatalf("acceptance cells = %d, want at least symmetric 10x2x3", len(matrix.Cells))
	}
	foundPending := false
	for _, cell := range matrix.Cells {
		if cell.Product == "codebuddy" && cell.Capability == "tencent-model-turn" && cell.Expected == "pending-external" {
			foundPending = true
		}
	}
	if !foundPending {
		t.Fatal("CodeBuddy account-gated cell was not projected")
	}
	if len(catalog) == 0 {
		t.Fatal("empty catalog")
	}
}

func TestProjectionIsStableAcrossAuthoredOrder(t *testing.T) {
	reversed := append([]descriptor(nil), authoredDescriptors...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	for _, view := range []string{"catalog", "release-inventory", "install-plan", "acceptance-matrix"} {
		first, err := projectionBytes(view, authoredDescriptors)
		if err != nil {
			t.Fatal(err)
		}
		second, err := projectionBytes(view, reversed)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("%s depends on authored ordering", view)
		}
	}
}

func TestSharedTokenGrammarRejectsProductAndFederationDrift(t *testing.T) {
	for _, value := range []string{"opencode", "opencode-lane", "dynamic-permission"} {
		if err := validateToken("shared", value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"OpenCode", "opencode_lane", "-lane", "opencode--lane", ""} {
		if err := validateToken("shared", value); err == nil {
			t.Fatalf("accepted invalid token %q", value)
		}
	}
	mutated := append([]descriptor(nil), authoredDescriptors...)
	mutated[0].FederationCapabilities = []string{"Bad_Capability"}
	if err := validateDescriptors(mutated); err == nil {
		t.Fatal("invalid federation token did not fail catalog validation")
	}
}

func TestProjectionDriftIsDetectable(t *testing.T) {
	baseline, err := projectionBytes("catalog", authoredDescriptors)
	if err != nil {
		t.Fatal(err)
	}
	mutated := append([]descriptor(nil), authoredDescriptors...)
	mutated[0].TestedVersion = "9.9.9"
	drifted, err := projectionBytes("catalog", mutated)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(baseline, drifted) {
		t.Fatal("descriptor drift did not change projection")
	}
	baseDigest, changedDigest := sha256.Sum256(baseline), sha256.Sum256(drifted)
	if hex.EncodeToString(baseDigest[:]) == hex.EncodeToString(changedDigest[:]) {
		t.Fatal("descriptor drift did not change digest")
	}
}

func TestDSHExactTupleAndExplicitParentCapability(t *testing.T) {
	for _, product := range authoredDescriptors {
		foundParent := false
		for _, capability := range product.Capabilities {
			if capability == CapabilityParent {
				foundParent = true
			}
		}
		if !foundParent {
			t.Fatalf("%s omitted explicit parent capability", product.ID)
		}
	}
	dsh := findDescriptor(authoredDescriptors, "dsh")
	if dsh == nil || dsh.Compatibility.PackageManager != "pnpm" || len(dsh.Compatibility.Tuple) != 3 {
		t.Fatalf("bad dsh tuple %#v", dsh)
	}
}

func TestCodeBuddySeparatesNativePeerAuthorityFromOwnedLaneAuthentication(t *testing.T) {
	product := findDescriptor(authoredDescriptors, "codebuddy")
	if product == nil || product.Authority == nil {
		t.Fatal("codebuddy authority metadata missing")
	}
	authority := product.Authority
	if authority.PeerLaunch != "managed-wrapper" || authority.PeerCredential != "none" || authority.PeerRequestHeader != "X-CodeBuddy-Request: 1" {
		t.Fatalf("incorrect peer authority %#v", authority)
	}
	if !hasAll(authority.PeerDiscovery, "native-registry-session-id", "native-registry-pid", "native-registry-url") ||
		!hasAll(authority.PeerAttestation, "literal-loopback", "socket-owner-pid", "executable-identity", "process-ancestry") {
		t.Fatalf("incomplete peer evidence %#v", authority)
	}
	if authority.LaneServerOwnership != "agent-sessions-owned" || authority.LaneAuthentication != "product-password-auth" || authority.LaneSecretLifetime != "memory-only" {
		t.Fatalf("incorrect lane authority %#v", authority)
	}
	if product.PeerTransport != "codebuddy-native-registry-http" || product.MessageTransport != "codebuddy-native-registry-http" {
		t.Fatalf("incorrect peer transport %q/%q", product.PeerTransport, product.MessageTransport)
	}
}
