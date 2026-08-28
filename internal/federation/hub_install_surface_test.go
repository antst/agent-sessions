package federation

import (
	"path/filepath"
	"testing"
)

func TestHubInstalledListenerIsStableInjectionSafeAndMatchesBoundWildcard(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "prefix")
	definition := filepath.Join(t.TempDir(), "agent-sessions-hub.service")
	if _, err := HubServiceRole(prefix, definition, ":0"); err == nil {
		t.Fatal("installed hub accepted an ephemeral listener port")
	}
	for _, listen := range []string{
		":7419\nEnvironment=INJECTED=true",
		"127.0.0.1:7419%h",
		"127.0.0.1:7419<key>",
	} {
		if _, err := HubServiceRole(prefix, definition, listen); err == nil {
			t.Fatalf("installed hub accepted service-definition injection %q", listen)
		}
	}
	if _, err := HubServiceRole(prefix, definition, ":7419"); err != nil {
		t.Fatalf("resolve stable wildcard hub listener: %v", err)
	}
	if !hubListenMatches(":7419", "[::]:7419") || !hubListenMatches(":7419", "0.0.0.0:7419") {
		t.Fatal("configured wildcard listener did not match its bound operating-system spelling")
	}
	if hubListenMatches("127.0.0.1:7419", "127.0.0.2:7419") || hubListenMatches(":7419", "[::]:7420") {
		t.Fatal("hub readiness accepted a different address or port")
	}
}
