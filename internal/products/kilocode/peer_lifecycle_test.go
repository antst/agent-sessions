package kilocode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/products/opencodefamily"
	"github.com/antst/agent-sessions/internal/productserver"
)

type kiloLifecycleComponents struct{}

func (kiloLifecycleComponents) LookupComponent(context.Context, string, string) (productruntime.ComponentSessionView, error) {
	return productruntime.ComponentSessionView{}, productruntime.ErrStale
}

type kiloLifecycleProcesses struct{ identity procinfo.Identity }

func (processes kiloLifecycleProcesses) CaptureIdentity(context.Context, int) (procinfo.Identity, error) {
	return processes.identity, nil
}
func (processes kiloLifecycleProcesses) ObserveIdentity(context.Context, procinfo.Identity) (procinfo.IdentityObservation, error) {
	return procinfo.IdentityObservation{Status: procinfo.IdentityMatches, Current: processes.identity}, nil
}
func (kiloLifecycleProcesses) Executable(context.Context, procinfo.Identity) (string, error) {
	return "/native/kilo", nil
}
func (kiloLifecycleProcesses) DescendsFrom(context.Context, procinfo.Identity, procinfo.Identity, int) (bool, error) {
	return true, nil
}

type kiloLifecycleGateway struct{}

func (kiloLifecycleGateway) Deliver(context.Context, string, string, productruntime.DeliveryRequest) (productruntime.NativeAcceptance, error) {
	return productruntime.NativeAcceptance{}, productruntime.ErrStale
}
func (kiloLifecycleGateway) Rename(context.Context, string, string, string) (productruntime.NativeName, error) {
	return productruntime.NativeName{}, productruntime.ErrStale
}

type unusedKiloServerManager struct{}

func (unusedKiloServerManager) Open(context.Context, opencodefamily.ServerOpenRequest) (*opencodefamily.LiveServer, error) {
	return nil, productruntime.ErrUnavailable
}
func (unusedKiloServerManager) Recover(context.Context, opencodefamily.ServerRecoveryRequest) (*opencodefamily.LiveServer, error) {
	return nil, productruntime.ErrUnavailable
}

type kiloLifecycleNative struct {
	t             *testing.T
	directory     string
	nativeID      string
	missingResume bool
	selectFalse   bool
	closeFailures int
	identity      procinfo.Identity

	server    *httptest.Server
	raw       *productserver.Client
	client    *opencodefamily.Client
	mu        sync.Mutex
	create    int
	get       int
	selects   int
	deletes   int
	closes    int
	attachIDs []string
}

func newKiloLifecycleNative(t *testing.T, directory, nativeID string) *kiloLifecycleNative {
	t.Helper()
	native := &kiloLifecycleNative{
		t: t, directory: directory, nativeID: nativeID,
		identity: procinfo.Identity{PID: 8123, Start: "start", StrongStart: "strong"},
	}
	native.server = httptest.NewServer(http.HandlerFunc(native.handle))
	auth, err := productserver.NewBasicAuth("agent-sessions", productruntime.NewSensitiveValue("lifecycle-secret"))
	if err != nil {
		t.Fatal(err)
	}
	native.raw, err = productserver.NewClient(productserver.ClientConfig{Endpoint: native.server.URL, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	native.client, err = opencodefamily.NewClient(opencodefamily.ClientConfig{
		HTTP: native.raw, Directory: directory, Dialect: opencodefamily.DialectKilo,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { native.raw.CloseIdleConnections(); native.server.Close() })
	return native
}

func (native *kiloLifecycleNative) handle(response http.ResponseWriter, request *http.Request) {
	username, password, ok := request.BasicAuth()
	if !ok || username != "agent-sessions" || password != "lifecycle-secret" || request.URL.Query().Get("directory") != native.directory {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	native.mu.Lock()
	defer native.mu.Unlock()
	switch request.Method + " " + request.URL.Path {
	case "POST /session":
		native.create++
		_ = json.NewEncoder(response).Encode(opencodefamily.Session{ID: native.nativeID})
	case "GET /session/" + native.nativeID:
		native.get++
		if native.missingResume {
			http.Error(response, "gone", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(response).Encode(opencodefamily.Session{ID: native.nativeID})
	case "POST /tui/select-session":
		native.selects++
		var body struct {
			SessionID string `json:"sessionID"`
		}
		if json.NewDecoder(request.Body).Decode(&body) != nil || body.SessionID != native.nativeID {
			http.Error(response, "wrong session", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(response).Encode(!native.selectFalse)
	case "DELETE /session/" + native.nativeID:
		native.deletes++
		_ = json.NewEncoder(response).Encode(true)
	default:
		http.NotFound(response, request)
	}
}

func (native *kiloLifecycleNative) Client() *opencodefamily.Client { return native.client }
func (native *kiloLifecycleNative) ProcessRef() productruntime.OwnedProcessRef {
	return productruntime.OwnedProcessRef{Process: native.identity, ProcessGroup: native.identity.PID}
}
func (native *kiloLifecycleNative) BuildKiloAttach(cwd, nativeID string, arguments []string, environment []productruntime.EnvVar) (productruntime.NativeCommand, error) {
	if cwd != native.directory || nativeID != native.nativeID {
		return productruntime.NativeCommand{}, productruntime.ErrAmbiguousSession
	}
	native.mu.Lock()
	native.attachIDs = append(native.attachIDs, nativeID)
	native.mu.Unlock()
	args := append([]string{"attach", "http://127.0.0.1:4096", "--dir", cwd, "--session", nativeID}, arguments...)
	return productruntime.NativeCommand{Path: "kilo", Args: args, Cwd: cwd, Env: environment}, nil
}
func (native *kiloLifecycleNative) Close(context.Context) error {
	native.mu.Lock()
	defer native.mu.Unlock()
	native.closes++
	if native.closes <= native.closeFailures {
		return errors.New("injected close failure")
	}
	return nil
}

func newKiloLifecycleDriver(t *testing.T, native *kiloLifecycleNative) (*PeerDriver, daemon.AttachmentAdapter, *int) {
	t.Helper()
	driver, err := NewPeerDriver(Config{
		Deps: productruntime.HostDeps{
			Generation: 3, Components: kiloLifecycleComponents{}, Processes: kiloLifecycleProcesses{identity: native.identity},
		},
		Gateway: kiloLifecycleGateway{}, Servers: unusedKiloServerManager{}, Executable: "/native/kilo",
	})
	if err != nil {
		t.Fatal(err)
	}
	opens := 0
	driver.openServer = func(_ context.Context, request opencodefamily.ServerOpenRequest) (kiloPeerServer, error) {
		opens++
		if request.Cwd != native.directory || request.Key == "" {
			return nil, productruntime.ErrAmbiguousSession
		}
		return native, nil
	}
	adapter, err := driver.AttachmentAdapter(productruntime.HostDeps{})
	if err != nil {
		t.Fatal(err)
	}
	return driver, adapter, &opens
}

func kiloLaunchRequest(attachmentID, cwd string) productruntime.PeerLaunchRequest {
	return productruntime.PeerLaunchRequest{
		ProductID: ProductID, AttachmentID: attachmentID, Cwd: cwd,
		BootstrapCapabilityID: "capability", BootstrapSecret: productruntime.NewSensitiveValue("bootstrap-secret"),
	}
}

func TestKiloPrepareBuildResumesOnlyExactStagedSession(t *testing.T) {
	native := newKiloLifecycleNative(t, "/work/resume", "ses_resume_exact")
	driver, adapter, opens := newKiloLifecycleDriver(t, native)
	attachment := daemon.ManagedAttachment{ID: "resume", Product: ProductID, Cwd: "/work/resume", NativeSessionID: "ses_resume_exact"}
	if _, err := adapter.Prepare(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	command, err := driver.BuildLaunch(context.Background(), kiloLaunchRequest(attachment.ID, attachment.Cwd))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"attach", "http://127.0.0.1:4096", "--dir", "/work/resume", "--session", "ses_resume_exact"}
	if !reflect.DeepEqual(command.Args, want) || native.get != 1 || native.create != 0 || native.selects != 1 || *opens != 1 {
		t.Fatalf("exact resume command=%v get=%d create=%d select=%d opens=%d", command.Args, native.get, native.create, native.selects, *opens)
	}
	if err := adapter.Rollback(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	if native.deletes != 0 || native.closes != 1 {
		t.Fatalf("resumed rollback deleted=%d closed=%d", native.deletes, native.closes)
	}
	restarted, restartedAdapter, restartedOpens := newKiloLifecycleDriver(t, native)
	if _, err := restartedAdapter.Prepare(context.Background(), attachment); err != nil {
		t.Fatalf("restart prepare = %v", err)
	}
	if _, err := restarted.BuildLaunch(context.Background(), kiloLaunchRequest(attachment.ID, attachment.Cwd)); err != nil {
		t.Fatalf("restart exact resume = %v", err)
	}
	if native.get != 2 || native.create != 0 || *restartedOpens != 1 {
		t.Fatalf("restart get=%d create=%d opens=%d", native.get, native.create, *restartedOpens)
	}
	if err := restartedAdapter.Rollback(context.Background(), attachment); err != nil {
		t.Fatalf("restart rollback = %v", err)
	}
}

func TestKiloPreparedCwdMismatchNeverOpensAndIntentCanRetryExact(t *testing.T) {
	native := newKiloLifecycleNative(t, "/work/exact", "ses_resume_exact")
	driver, adapter, opens := newKiloLifecycleDriver(t, native)
	attachment := daemon.ManagedAttachment{ID: "cwd", Product: ProductID, Cwd: "/work/exact", NativeSessionID: "ses_resume_exact"}
	if _, err := adapter.Prepare(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.BuildLaunch(context.Background(), kiloLaunchRequest(attachment.ID, "/work/foreign")); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("cwd mismatch = %v", err)
	}
	if *opens != 0 {
		t.Fatalf("cwd mismatch opened %d servers", *opens)
	}
	if _, err := driver.BuildLaunch(context.Background(), kiloLaunchRequest(attachment.ID, attachment.Cwd)); err != nil {
		t.Fatalf("exact retry = %v", err)
	}
}

func TestKiloMissingResumeNeverCreatesOrFallsBack(t *testing.T) {
	native := newKiloLifecycleNative(t, "/work/gone", "ses_gone")
	native.missingResume = true
	native.closeFailures = 1
	driver, adapter, _ := newKiloLifecycleDriver(t, native)
	attachment := daemon.ManagedAttachment{ID: "gone", Product: ProductID, Cwd: native.directory, NativeSessionID: native.nativeID}
	if _, err := adapter.Prepare(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.BuildLaunch(context.Background(), kiloLaunchRequest(attachment.ID, attachment.Cwd)); !errors.Is(err, productruntime.ErrStale) || !errors.Is(err, productruntime.ErrCleanupDebt) {
		t.Fatalf("missing exact resume = %v", err)
	}
	if native.create != 0 || native.selects != 0 || native.closes != 1 {
		t.Fatalf("missing resume create=%d select=%d close=%d", native.create, native.selects, native.closes)
	}
	if err := adapter.Rollback(context.Background(), attachment); err != nil {
		t.Fatalf("missing resume cleanup retry = %v", err)
	}
	if native.create != 0 || native.deletes != 0 || native.closes != 2 {
		t.Fatalf("missing resume retry create=%d delete=%d close=%d", native.create, native.deletes, native.closes)
	}
}

func TestKiloFreshFailureDeletesExactlyAndRollbackRetriesCleanupDebt(t *testing.T) {
	native := newKiloLifecycleNative(t, "/work/fresh", "ses_fresh")
	native.selectFalse = true
	native.closeFailures = 1
	driver, adapter, _ := newKiloLifecycleDriver(t, native)
	attachment := daemon.ManagedAttachment{ID: "fresh-fail", Product: ProductID, Cwd: native.directory}
	if _, err := adapter.Prepare(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.BuildLaunch(context.Background(), kiloLaunchRequest(attachment.ID, attachment.Cwd)); !errors.Is(err, productruntime.ErrCleanupDebt) {
		t.Fatalf("fresh cleanup debt = %v", err)
	}
	if native.create != 1 || native.deletes != 1 || native.closes != 1 {
		t.Fatalf("first cleanup create=%d delete=%d close=%d", native.create, native.deletes, native.closes)
	}
	if err := adapter.Rollback(context.Background(), attachment); err != nil {
		t.Fatalf("cleanup retry = %v", err)
	}
	if native.deletes != 1 || native.closes != 2 {
		t.Fatalf("retry cleanup delete=%d close=%d", native.deletes, native.closes)
	}
}

func TestKiloFreshAttachedDetachPreservesNativeSessionAndMismatchCannotAdopt(t *testing.T) {
	native := newKiloLifecycleNative(t, "/work/fresh", "ses_fresh")
	driver, adapter, _ := newKiloLifecycleDriver(t, native)
	attachment := daemon.ManagedAttachment{ID: "fresh", Product: ProductID, Cwd: native.directory}
	if _, err := adapter.Prepare(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.BuildLaunch(context.Background(), kiloLaunchRequest(attachment.ID, attachment.Cwd)); err != nil {
		t.Fatal(err)
	}
	observed := daemon.NativeEvidence{
		Process: native.identity, ThreadID: "ses_foreign", ArtifactType: "component", ArtifactRevision: IntegrationVersion,
	}
	if _, err := adapter.Adopt(context.Background(), attachment, observed); !errors.Is(err, productruntime.ErrUnauthorized) {
		t.Fatalf("foreign native adopt = %v", err)
	}
	attachment.NativeSessionID = native.nativeID
	observed.ThreadID = native.nativeID
	if _, err := adapter.Adopt(context.Background(), attachment, observed); err != nil {
		t.Fatalf("exact adopt = %v", err)
	}
	if err := adapter.Detach(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	if native.deletes != 0 || native.closes != 1 {
		t.Fatalf("attached detach deleted=%d closed=%d", native.deletes, native.closes)
	}
}

func TestKiloRollbackDuringLaunchReturnsDebtAndEventualCleanupRemainsRetryable(t *testing.T) {
	native := newKiloLifecycleNative(t, "/work/race", "ses_race_fresh")
	native.closeFailures = 1
	driver, adapter, _ := newKiloLifecycleDriver(t, native)
	entered := make(chan struct{})
	release := make(chan struct{})
	driver.openServer = func(context.Context, opencodefamily.ServerOpenRequest) (kiloPeerServer, error) {
		close(entered)
		<-release
		return native, nil
	}
	attachment := daemon.ManagedAttachment{ID: "race", Product: ProductID, Cwd: native.directory}
	if _, err := adapter.Prepare(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	buildResult := make(chan error, 1)
	go func() {
		_, err := driver.BuildLaunch(context.Background(), kiloLaunchRequest(attachment.ID, attachment.Cwd))
		buildResult <- err
	}()
	<-entered
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := adapter.Rollback(rollbackCtx, attachment); !errors.Is(err, productruntime.ErrCleanupDebt) {
		t.Fatalf("in-flight rollback = %v", err)
	}
	close(release)
	if err := <-buildResult; err != nil {
		t.Fatalf("build after bounded rollback = %v", err)
	}
	if err := adapter.Rollback(context.Background(), attachment); !errors.Is(err, productruntime.ErrCleanupDebt) {
		t.Fatalf("first exact cleanup retry = %v", err)
	}
	if native.deletes != 1 || native.closes != 1 {
		t.Fatalf("first retry delete=%d close=%d", native.deletes, native.closes)
	}
	if err := adapter.Rollback(context.Background(), attachment); err != nil {
		t.Fatalf("second exact cleanup retry = %v", err)
	}
	if native.deletes != 1 || native.closes != 2 {
		t.Fatalf("second retry delete=%d close=%d", native.deletes, native.closes)
	}
}
