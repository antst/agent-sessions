package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/clihelp"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

func TestHostStatusSchemaCarriesExactAuthorityAndIsolatesOptionalAdapters(t *testing.T) {
	runtime := newAdminTestRuntime(t)
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("start host runtime with an unavailable optional adapter: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })

	body, err := json.Marshal(runtime.StatusProjection())
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatal(err)
	}
	assertExactStringSet(t, "status JSON fields", mapKeys(status), clihelp.Contract().JSONResultFields["status"])
	wantIdentity := map[string]any{
		"runtime_version": "0.3.0-test", "runtime_identity": "sha256:t023-runtime",
		"generation": float64(1), "pid": float64(4242), "proc_start": "t023-process-start",
		"endpoint": runtime.options.Paths.ControlEndpoint,
	}
	for field, want := range wantIdentity {
		if got := status[field]; !reflect.DeepEqual(got, want) {
			t.Errorf("status %s = %#v, want %#v", field, got, want)
		}
	}
	products, ok := status["products"].(map[string]any)
	if !ok {
		t.Fatalf("status products = %#v", status["products"])
	}
	wantProducts := make([]string, 0, len(productcatalog.Catalog().Products))
	for _, product := range productcatalog.Catalog().Products {
		wantProducts = append(wantProducts, product.ID)
	}
	assertExactStringSet(t, "status products", mapKeys(products), wantProducts)
	allowedStates := []string{"not_installed", "installed_unready", "ready", "degraded"}
	for _, product := range wantProducts {
		entry, entryOK := products[product].(map[string]any)
		if !entryOK || !slices.Contains(allowedStates, entry["state"].(string)) {
			t.Errorf("status product %s = %#v", product, products[product])
		}
	}
}

func TestHostDoctorSchemaIsCompleteAndReadOnly(t *testing.T) {
	runtime := newAdminTestRuntime(t)
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Stop(context.Background()) })
	body, err := json.Marshal(runtime.DoctorProjection())
	if err != nil {
		t.Fatal(err)
	}
	var doctor map[string]any
	if err := json.Unmarshal(body, &doctor); err != nil {
		t.Fatal(err)
	}
	assertExactStringSet(t, "doctor JSON fields", mapKeys(doctor), clihelp.Contract().JSONResultFields["doctor"])
	if healthy, ok := doctor["healthy"].(bool); !ok || !healthy {
		t.Errorf("doctor healthy = %#v", doctor["healthy"])
	}
}

func TestAdministrativeOperationsAreCompleteAndAbsentFromMCPConnectorRole(t *testing.T) {
	wantAdmin := []string{"remove.inspect", "runtime.doctor", "runtime.status"}
	assertExactStringSet(t, "admin operations", setKeys(controlRoleOperations[controlRoleAdmin]), wantAdmin)
	for _, operation := range setKeys(controlRoleOperations[controlRoleConnector]) {
		if strings.HasPrefix(operation, "runtime.") || strings.HasPrefix(operation, "remove.") ||
			strings.HasPrefix(operation, "purge.") {
			t.Errorf("model-facing MCP connector exposes administration operation %q", operation)
		}
	}
}

func newAdminTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	state, err := OpenStateStore(stateRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	paths := ProductionPaths{
		ConfigurationRoot: filepath.Join(root, "config"), ConfigurationFile: filepath.Join(root, "config", "config.json"),
		StateRoot: stateRoot, RuntimeRoot: filepath.Join(root, "run"), ControlEndpoint: filepath.Join(root, "run", "daemon.sock"),
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	overrides := make(map[string]ProductOverride)
	for _, product := range productcatalog.Catalog().Products {
		executable := filepath.Join(bin, product.ID)
		if product.ID != "qwen" {
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		overrides[product.ID] = ProductOverride{Executable: executable}
	}
	configuration := DaemonConfig{
		SchemaVersion: DaemonConfigSchemaVersion, HostID: "t023-host", HostName: "t023-builder",
		ProductOverrides: overrides, StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: 1,
	}
	runtime, err := NewRuntime(RuntimeOptions{
		Paths: paths, Configuration: configuration, State: state,
		RuntimeVersion: "0.3.0-test", RuntimeIdentity: "sha256:t023-runtime", PID: 4242,
		ProcStart: "t023-process-start", StrongStart: "t023-strong-start",
		ServiceManager: "systemd-user", ServiceUnit: "agent-sessions.service",
		Now: func() time.Time { return time.UnixMilli(1000) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func mapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func setKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func assertExactStringSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	got, want = append([]string(nil), got...), append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s = %q, want exactly %q", label, got, want)
	}
}
