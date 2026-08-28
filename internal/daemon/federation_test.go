package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/federation"
	"github.com/antst/agent-sessions/internal/statestore"
)

func TestAdoptedFederationSeedUsesCurrentReadyAdvertisement(t *testing.T) {
	prior := FederationStateRecord{
		RecordHeader: RecordHeader{
			SchemaVersion: HostRuntimeSchemaVersion, Revision: 1, Generation: 1,
			CreatedAt: 100, UpdatedAt: 100,
		},
		HostID: "host-a", HostName: "host-a", HubAddress: "hub.example.test:7443",
		ConnectionGeneration: 1, ProtocolVersion: federation.ProtocolVersion, State: "reconnecting",
		AdvertisedProducts: []string{"claude", "codex", "grok", "qwen"},
	}
	products := []string{"codex", "qwen"}
	capabilities := []string{"codex-lane", "qwen-lane"}
	runtimeIdentity := "sha256:" + strings.Repeat("a", 64)
	normalized := normalizeAdoptedFederationSeed(
		prior, 0, products, capabilities, "0.3.0", runtimeIdentity,
	)
	if err := validateFederationStateRecord(normalized); err != nil {
		t.Fatalf("normalized adopted federation seed: %v", err)
	}
	if !reflect.DeepEqual(normalized.AdvertisedProducts, products) ||
		!reflect.DeepEqual(normalized.AdvertisedCapabilities, capabilities) ||
		normalized.AdvertisedRuntimeVersion != "0.3.0" ||
		normalized.AdvertisedRuntimeIdentity != runtimeIdentity {
		t.Fatalf("normalized adopted advertisement = %+v", normalized)
	}

	ordinary := normalizeAdoptedFederationSeed(
		prior, statestore.Revision(1), products, capabilities, "0.3.0", runtimeIdentity,
	)
	if !reflect.DeepEqual(ordinary, prior) {
		t.Fatalf("ordinary federation revision was implicitly repaired: %+v", ordinary)
	}
	if err := validateFederationStateRecord(ordinary); err == nil {
		t.Fatal("invalid ordinary federation revision did not remain fail-closed")
	}
}

func TestEmbeddedFederationAdvertisesStableHostIdentityAndOnlyReadyProducts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	advertisements := make(chan federation.HostAdvertisement, 3)
	states := make(chan FederationStateRecord, 8)
	var backoffs []time.Duration
	component, err := newFederationComponent(federationComponentOptions{
		HostID:          "host-stable",
		HostName:        "workstation",
		HubAddress:      "hub.example.test:7443",
		RuntimeVersion:  "2026.08-host",
		RuntimeIdentity: "sha256:" + strings.Repeat("a", 64),
		Generation:      17,
		ReadyProducts: func(context.Context) []string {
			return []string{"qwen", "codex"}
		},
		RunSession: func(ctx context.Context, advertisement federation.HostAdvertisement, connected func(string) error) error {
			advertisements <- advertisement
			if err := connected("roster-9"); err != nil {
				return err
			}
			if len(advertisements) == 1 {
				return syscall.ECONNRESET
			}
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
		WaitRetry: func(_ context.Context, delay time.Duration) error {
			backoffs = append(backoffs, delay)
			return nil
		},
		PublishState: func(_ context.Context, state FederationStateRecord) error {
			states <- state
			return nil
		},
	})
	if err != nil {
		t.Fatalf("create embedded federation component: %v", err)
	}
	if err := component.Run(ctx); err != nil {
		t.Fatalf("run embedded federation component: %v", err)
	}
	close(advertisements)
	gotAdvertisements := make([]federation.HostAdvertisement, 0, 2)
	for advertisement := range advertisements {
		gotAdvertisements = append(gotAdvertisements, advertisement)
	}
	if len(gotAdvertisements) != 2 {
		t.Fatalf("session advertisements = %d, want initial connection and reconnect", len(gotAdvertisements))
	}
	wantProducts := []string{"codex", "qwen"}
	wantCapabilities := []string{"codex-lane", "qwen-lane"}
	for _, advertisement := range gotAdvertisements {
		if advertisement.HostID != "host-stable" || advertisement.HostName != "workstation" ||
			advertisement.ProtocolVersion != federation.ProtocolVersion || advertisement.Generation != 17 ||
			advertisement.RuntimeVersion != "2026.08-host" ||
			advertisement.RuntimeIdentity != "sha256:"+strings.Repeat("a", 64) {
			t.Fatalf("advertisement lost exact daemon identity: %+v", advertisement)
		}
		if !reflect.DeepEqual(advertisement.Products, wantProducts) ||
			!reflect.DeepEqual(advertisement.Capabilities, wantCapabilities) {
			t.Fatalf("ready product advertisement = products %q capabilities %q", advertisement.Products, advertisement.Capabilities)
		}
	}
	if !reflect.DeepEqual(backoffs, []time.Duration{250 * time.Millisecond}) {
		t.Fatalf("reconnect backoff = %v, want bounded initial backoff", backoffs)
	}
	close(states)
	var sawConnected, sawRetry bool
	var lastConnectionGeneration uint64
	for state := range states {
		if state.HostID != "host-stable" || state.HostName != "workstation" || state.HubAddress != "hub.example.test:7443" {
			t.Fatalf("published federation state changed configured identity: %+v", state)
		}
		if state.State == "connected" {
			sawConnected = true
			if state.ConnectionGeneration <= lastConnectionGeneration {
				t.Fatalf("connection generation did not advance: prior=%d current=%d", lastConnectionGeneration, state.ConnectionGeneration)
			}
			lastConnectionGeneration = state.ConnectionGeneration
			if state.RemoteRosterRevision != "roster-9" ||
				!reflect.DeepEqual(state.AdvertisedProducts, wantProducts) ||
				!reflect.DeepEqual(state.AdvertisedCapabilities, wantCapabilities) {
				t.Fatalf("connected state omitted exact advertisement evidence: %+v", state)
			}
		}
		if state.State == "reconnecting" && state.LastErrorCode == "hub_connection_lost" {
			sawRetry = true
		}
	}
	if !sawConnected || !sawRetry {
		t.Fatalf("federation lifecycle states connected=%t retry=%t", sawConnected, sawRetry)
	}
}

func TestEmbeddedFederationSuccessorKeepsHostIdentityAndPublishesNewRuntimeGeneration(t *testing.T) {
	capture := func(generation uint64, version, identity string) federation.HostAdvertisement {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var got federation.HostAdvertisement
		component, err := newFederationComponent(federationComponentOptions{
			HostID: "host-stable", HostName: "workstation", HubAddress: "hub.example.test:7443",
			RuntimeVersion: version, RuntimeIdentity: identity, Generation: generation,
			ReadyProducts: func(context.Context) []string { return []string{"codex"} },
			RunSession: func(ctx context.Context, advertisement federation.HostAdvertisement, connected func(string) error) error {
				got = advertisement
				if err := connected("roster-current"); err != nil {
					return err
				}
				cancel()
				<-ctx.Done()
				return ctx.Err()
			},
			WaitRetry:    func(context.Context, time.Duration) error { return nil },
			PublishState: func(context.Context, FederationStateRecord) error { return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := component.Run(ctx); err != nil {
			t.Fatal(err)
		}
		return got
	}

	prior := capture(81, "host-release-a", "sha256:"+strings.Repeat("1", 64))
	successor := capture(82, "unrelated-host-release-b", "sha256:"+strings.Repeat("f", 64))
	if prior.HostID != successor.HostID || prior.HostName != successor.HostName {
		t.Fatalf("successor replaced durable host identity: prior=%+v successor=%+v", prior, successor)
	}
	if successor.Generation != prior.Generation+1 || successor.RuntimeVersion == prior.RuntimeVersion ||
		successor.RuntimeIdentity == prior.RuntimeIdentity {
		t.Fatalf("successor did not replace exact runtime generation: prior=%+v successor=%+v", prior, successor)
	}
}

func TestFederationHubOutageIsRetryableBeforeAcceptanceWithoutLocalFallback(t *testing.T) {
	localDispatches := 0
	component, err := newFederationComponent(federationComponentOptions{
		HostID: "host-a", HostName: "host-a", HubAddress: "127.0.0.1:1",
		RuntimeVersion: "test", RuntimeIdentity: "sha256:" + strings.Repeat("2", 64), Generation: 1,
		ReadyProducts: func(context.Context) []string { return nil },
		RunSession: func(context.Context, federation.HostAdvertisement, func(string) error) error {
			return syscall.ECONNREFUSED
		},
		WaitRetry:    func(context.Context, time.Duration) error { return nil },
		PublishState: func(context.Context, FederationStateRecord) error { return nil },
		SendRemote: func(context.Context, federationDispatchRequest) error {
			return syscall.ECONNREFUSED
		},
		DispatchLocal: func(context.Context, federation.AgentFrame) error {
			localDispatches++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := component.DispatchRemote(context.Background(), federationDispatchRequest{
		RequestID: "remote-request-1", Operation: "group_deliver",
		Frame: federation.AgentFrame{Version: federation.AgentFrameVersion, Type: "delivery", MessageID: "message-1"},
	})
	if err == nil || result.Accepted {
		t.Fatalf("hub outage dispatch = result %+v error %v, want refusal before acceptance", result, err)
	}
	var refusal interface {
		Accepted() bool
		Retryable() bool
	}
	if !errors.As(err, &refusal) || refusal.Accepted() || !refusal.Retryable() {
		t.Fatalf("hub outage error is not an explicit retryable pre-acceptance refusal: %T %v", err, err)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("hub outage lost real cause/address: %v", err)
	}
	if localDispatches != 0 {
		t.Fatalf("hub outage dispatched %d times through a local fallback", localDispatches)
	}
}

func TestEmbeddedFederationRecoversConnectionGenerationAndReadvertisesCurrentReadyProducts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state, err := OpenStateStore(filepath.Join(t.TempDir(), "state"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	prior := FederationStateRecord{
		RecordHeader: RecordHeader{
			SchemaVersion: HostRuntimeSchemaVersion, Revision: 7, Generation: 41,
			CreatedAt: 100, UpdatedAt: 150,
		},
		HostID: "host-stable", HostName: "workstation", HubAddress: "hub.example.test:7443",
		ConnectionGeneration: 9, ProtocolVersion: federation.ProtocolVersion,
		AdvertisedCapabilities: []string{"claude-lane"}, State: string(federation.AgentBackoff),
		AdvertisedRuntimeVersion: "prior-release", AdvertisedRuntimeIdentity: "sha256:prior",
		AdvertisedProducts: []string{"claude"}, RemoteRosterRevision: "roster-8",
		LastConnectedAt: 140, LastErrorCode: "hub_connection_lost",
	}
	if _, err := state.compareAndSwapFederation(ctx, 0, prior); err != nil {
		t.Fatal(err)
	}

	var advertisement federation.HostAdvertisement
	component, err := newFederationComponent(federationComponentOptions{
		configuration: DaemonConfig{
			HostID: "host-stable", HostName: "workstation", HubAddress: "hub.example.test:7443",
			RemoteLanesEnabled: true,
		},
		generation: 42, runtimeVersion: "current-release", runtimeIdentity: "sha256:current",
		state: state, now: func() time.Time { return time.UnixMilli(200) },
		ReadyProducts: func(context.Context) []string { return []string{"codex"} },
		RunSession: func(ctx context.Context, got federation.HostAdvertisement, connected func(string) error) error {
			advertisement = got
			if err := connected("roster-9"); err != nil {
				return err
			}
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
		WaitRetry: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := component.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(advertisement.Products, []string{"codex"}) ||
		!reflect.DeepEqual(advertisement.Capabilities, []string{"codex-lane"}) ||
		advertisement.Generation != 42 || advertisement.RuntimeVersion != "current-release" ||
		advertisement.RuntimeIdentity != "sha256:current" {
		t.Fatalf("successor advertisement = %+v", advertisement)
	}
	got, _, err := state.ReadFederation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != 42 || got.ConnectionGeneration != 10 || got.State != "connected" ||
		got.RemoteRosterRevision != "roster-9" ||
		!reflect.DeepEqual(got.AdvertisedProducts, []string{"codex"}) ||
		!reflect.DeepEqual(got.AdvertisedCapabilities, []string{"codex-lane"}) {
		t.Fatalf("recovered federation state = %+v", got)
	}
}

func TestHostFederationStatusAndDoctorExposeBoundedConnectionMetadata(t *testing.T) {
	runtime := newFederationDiagnosticsRuntime(t)
	runtime.options.Configuration.HubAddress = "hub.example.test:7443"
	runtime.options.Configuration.RemoteLanesEnabled = true
	runtime.generation = 12
	runtime.admission = AdmissionReady
	runtime.federation = &federationComponent{status: FederationStateRecord{
		RecordHeader: RecordHeader{
			SchemaVersion: HostRuntimeSchemaVersion, Revision: 4, Generation: 12,
			CreatedAt: 100, UpdatedAt: 200,
		},
		HostID: runtime.options.Configuration.HostID, HostName: runtime.options.Configuration.HostName,
		HubAddress: "hub.example.test:7443", ConnectionGeneration: 3,
		ProtocolVersion: federation.ProtocolVersion, AdvertisedCapabilities: []string{"codex-lane"},
		State: string(federation.AgentConnected), AdvertisedRuntimeVersion: runtime.options.RuntimeVersion,
		AdvertisedRuntimeIdentity: runtime.options.RuntimeIdentity, AdvertisedProducts: []string{"codex"},
		RemoteRosterRevision: "roster-11", LastConnectedAt: 190,
	}}

	status := runtime.StatusProjection()
	body, err := json.Marshal(status.Federation)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		`"state":"connected"`, `"host_id":"t023-host"`, `"hub_address":"hub.example.test:7443"`,
		`"protocol_version":3`, `"connection_generation":3`, `"advertised_products":["codex"]`,
		`"advertised_capabilities":["codex-lane"]`, `"remote_roster_revision":"roster-11"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("host federation status omitted %s: %s", want, text)
		}
	}
	if doctor := runtime.DoctorProjection(); !doctor.Healthy {
		t.Fatalf("connected federation doctor = %+v", doctor)
	}

	runtime.federation.status.State = string(federation.AgentIncompatible)
	runtime.federation.status.LastErrorCode = "protocol_mismatch"
	doctor := runtime.DoctorProjection()
	if doctor.Healthy {
		t.Fatalf("protocol mismatch doctor = %+v", doctor)
	}
	for _, check := range doctor.Checks {
		if check.ID == "federation_protocol" {
			if check.ErrorCode != "federation_protocol_mismatch" || !strings.Contains(check.NextAction, "matching") {
				t.Fatalf("protocol mismatch check = %+v", check)
			}
			return
		}
	}
	t.Fatal("doctor omitted federation protocol check")
}

func newFederationDiagnosticsRuntime(t *testing.T) *Runtime {
	t.Helper()
	root := t.TempDir()
	paths := ProductionPaths{
		StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "run"),
		ControlEndpoint: filepath.Join(root, "run", "daemon.sock"),
	}
	state, err := OpenStateStore(paths.StateRoot, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(RuntimeOptions{
		Paths: paths,
		Configuration: DaemonConfig{
			SchemaVersion: DaemonConfigSchemaVersion, HostID: "t023-host", HostName: "t023-builder",
			StateRoot: paths.StateRoot, RuntimeRoot: paths.RuntimeRoot, Revision: 1, UpdatedAt: 1,
		},
		State: state, RuntimeVersion: "0.3.0-test", RuntimeIdentity: "sha256:t023-runtime", PID: 4242,
		ProcStart: "t023-process-start", StrongStart: "t023-strong-start",
		ServiceManager: "systemd-user", ServiceUnit: "agent-sessions.service",
		Now: func() time.Time { return time.UnixMilli(1000) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}
