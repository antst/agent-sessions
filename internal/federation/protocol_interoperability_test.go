package federation

import (
	"reflect"
	"strings"
	"testing"
)

func TestArbitrarySHABuildsInteroperateByProtocolVersionOnly(t *testing.T) {
	builds := []struct {
		release string
		sha     string
		age     string
	}{
		{release: "2026.08-host", sha: strings.Repeat("1", 40), age: "newer"},
		{release: "2024.01-hub", sha: strings.Repeat("f", 40), age: "older"},
		{release: "unrelated-development-build", sha: strings.Repeat("a5", 20), age: "unknown"},
	}
	for _, host := range builds {
		for _, hub := range builds {
			t.Run(host.release+"/to/"+hub.release, func(t *testing.T) {
				if !CompatibleProtocol(ProtocolVersion) {
					t.Fatalf("equal protocol rejected host=%+v hub=%+v", host, hub)
				}
				registry := NewHubRegistry()
				if _, err := registry.RegisterHost(HostAdvertisement{
					HostID: "host-a", HostName: "host-a", ProtocolVersion: ProtocolVersion,
					RuntimeVersion: host.release, RuntimeIdentity: "sha256:" + host.sha,
					Generation: 1,
				}); err != nil {
					t.Fatalf("hub build %+v release-coupled host build %+v: %v", hub, host, err)
				}
				if snapshot := registry.Snapshot(); len(snapshot.Hosts) != 1 || snapshot.Hosts[0].ID != "host-a" {
					t.Fatalf("equal-protocol arbitrary-SHA registration = %+v", snapshot)
				}
			})
		}
	}
}

func TestCapabilitiesGateOperationsWithoutCouplingReleases(t *testing.T) {
	normalized := NormalizeCapabilities([]string{
		"qwen-lane", "unknown-future-lane", "codex-lane", "qwen-lane",
	})
	want := []string{"codex-lane", "qwen-lane"}
	if !reflect.DeepEqual(normalized, want) {
		t.Fatalf("normalized capabilities = %q, want %q", normalized, want)
	}
	host := Host{ID: "host-a", Name: "host-a", Capabilities: normalized}
	if !HostSupportsCapability(host, "codex-lane") || HostSupportsCapability(host, "claude-lane") {
		t.Fatalf("capability gate = %#v", host.Capabilities)
	}
	if !CompatibleProtocol(ProtocolVersion) {
		t.Fatal("missing operation capability changed protocol compatibility")
	}
	registry := NewHubRegistry()
	if _, err := registry.RegisterHost(HostAdvertisement{
		HostID: "host-a", HostName: "host-a", ProtocolVersion: ProtocolVersion,
		RuntimeVersion: "unrelated-release", RuntimeIdentity: "sha256:" + strings.Repeat("7", 64),
		Generation: 1, Products: []string{"codex", "qwen"},
		Capabilities: []string{"qwen-lane", "unknown-future-lane", "codex-lane", "qwen-lane"},
	}); err != nil {
		t.Fatalf("unknown/duplicate capability coupled host registration: %v", err)
	}
	registered, ok := registry.Host("host-a")
	if !ok || !reflect.DeepEqual(registered.Capabilities, want) {
		t.Fatalf("registered operation availability = %+v, want %q", registered, want)
	}
}

func TestProtocolMismatchNamesRequiredVersionBeforeRegistration(t *testing.T) {
	for _, remote := range []int{ProtocolVersion - 1, ProtocolVersion + 1} {
		registry := NewHubRegistry()
		before := registry.Snapshot()
		_, err := registry.RegisterHost(HostAdvertisement{
			HostID: "host-mismatch", HostName: "host-mismatch", ProtocolVersion: remote,
			RuntimeVersion: "release-any", RuntimeIdentity: "sha256:" + strings.Repeat("8", 64), Generation: 1,
		})
		if err == nil {
			t.Fatalf("protocol %d was accepted", remote)
		}
		text := err.Error()
		if !strings.Contains(text, "protocol") || !strings.Contains(text, "3") || !strings.Contains(text, "matching") {
			t.Fatalf("protocol %d diagnostic = %q, want required matching version", remote, text)
		}
		if after := registry.Snapshot(); !reflect.DeepEqual(after, before) {
			t.Fatalf("protocol %d mutated registry before refusal: before=%+v after=%+v", remote, before, after)
		}
	}
}
