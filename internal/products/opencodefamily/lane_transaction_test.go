package opencodefamily

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type laneTransactionManager struct {
	client *Client

	mu             sync.Mutex
	openCalls      int
	closeCalls     int
	closeFailures  int
	openErr        error
	openEntered    chan struct{}
	openRelease    chan struct{}
	openSignalOnce sync.Once
}

func (manager *laneTransactionManager) live() *LiveServer {
	return &LiveServer{client: manager.client, closeFn: func(context.Context) error {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		manager.closeCalls++
		if manager.closeCalls <= manager.closeFailures {
			return errors.New("injected lane close failure")
		}
		return nil
	}}
}

func (manager *laneTransactionManager) Open(context.Context, ServerOpenRequest) (*LiveServer, error) {
	manager.mu.Lock()
	manager.openCalls++
	err := manager.openErr
	entered, release := manager.openEntered, manager.openRelease
	manager.mu.Unlock()
	if entered != nil {
		manager.openSignalOnce.Do(func() { close(entered) })
		<-release
	}
	return manager.live(), err
}

func newTransactionLaneDriver(t *testing.T, manager ServerManager) *LaneDriver {
	t.Helper()
	config := LaneConfig{
		ProductID: "opencode", Dialect: DialectOpenCode, Generation: 7,
		Servers: manager, MapPermission: MapPermissionRules,
	}
	driver, err := NewLaneDriver(config)
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func TestLaneOpenRetainsServerReturnedWithErrorUntilCleanupRetry(t *testing.T) {
	manager := &laneTransactionManager{openErr: productruntime.ErrCleanupDebt, closeFailures: 1}
	driver := newTransactionLaneDriver(t, manager)
	request := productruntime.LaneOpenRequest{
		ProductID: "opencode", LaneID: "open-error", Cwd: "/work/project", PermissionMode: permissionmode.Default,
	}
	if _, err := driver.Open(context.Background(), request); !errors.Is(err, productruntime.ErrCleanupDebt) {
		t.Fatalf("server-plus-error open = %v", err)
	}
	manager.mu.Lock()
	manager.openErr = nil
	openCalls, closeCalls := manager.openCalls, manager.closeCalls
	manager.mu.Unlock()
	if openCalls != 1 || closeCalls != 1 {
		t.Fatalf("first open calls=%d close=%d", openCalls, closeCalls)
	}
	if _, err := driver.Open(context.Background(), request); !errors.Is(err, productruntime.ErrCleanupDebt) {
		t.Fatalf("cleanup convergence marker = %v", err)
	}
	manager.mu.Lock()
	openCalls, closeCalls = manager.openCalls, manager.closeCalls
	manager.mu.Unlock()
	if openCalls != 1 || closeCalls != 2 || driver.lanes[request.LaneID] != nil {
		t.Fatalf("retry open calls=%d close=%d retained=%v", openCalls, closeCalls, driver.lanes[request.LaneID] != nil)
	}
}

func TestFreshProvisionalLaneDeletesExactlyAndRetainsCloseDebt(t *testing.T) {
	deleteCalls := 0
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		if request.Method == http.MethodDelete && request.URL.Path == "/session/ses_fresh_cleanup" {
			deleteCalls++
			_, _ = response.Write([]byte("true"))
			return
		}
		http.NotFound(response, request)
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()
	manager := &laneTransactionManager{client: client, closeFailures: 1}
	driver := newTransactionLaneDriver(t, manager)
	live := &laneSession{
		server: manager.live(), client: client, nativeID: "ses_fresh_cleanup", permissionMode: permissionmode.Default,
		provisional: true, opening: true, openDone: make(chan struct{}), fresh: true,
	}
	driver.lanes["fresh-cleanup"] = live
	if err := driver.failProvisionalOpen(context.Background(), "fresh-cleanup", live, productruntime.ErrProtocol); !errors.Is(err, productruntime.ErrCleanupDebt) {
		t.Fatalf("fresh cleanup debt = %v", err)
	}
	if deleteCalls != 1 || driver.lanes["fresh-cleanup"] != live {
		t.Fatalf("first cleanup deletes=%d retained=%v", deleteCalls, driver.lanes["fresh-cleanup"] == live)
	}
	if err := driver.retryProvisionalCleanup(context.Background(), "fresh-cleanup", live); err != nil {
		t.Fatalf("fresh cleanup retry = %v", err)
	}
	manager.mu.Lock()
	closeCalls := manager.closeCalls
	manager.mu.Unlock()
	if deleteCalls != 1 || closeCalls != 2 || driver.lanes["fresh-cleanup"] != nil {
		t.Fatalf("cleanup convergence deletes=%d closes=%d retained=%v", deleteCalls, closeCalls, driver.lanes["fresh-cleanup"] != nil)
	}
}

func TestConcurrentLaneOpenIsReservedBeforeNativeServerStart(t *testing.T) {
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requireBasicAuth(t, request)
		if request.Method == http.MethodPost && request.URL.Path == "/session" {
			_, _ = response.Write([]byte(`{"id":"ses_reserved"}`))
			return
		}
		http.NotFound(response, request)
	})
	client, closeClient := newFamilyTestClient(t, DialectOpenCode, handler)
	defer closeClient()
	manager := &laneTransactionManager{client: client, openEntered: make(chan struct{}), openRelease: make(chan struct{})}
	driver := newTransactionLaneDriver(t, manager)
	request := productruntime.LaneOpenRequest{
		ProductID: "opencode", LaneID: "reserved", Cwd: "/work/project", PermissionMode: permissionmode.Default,
	}
	first := make(chan error, 1)
	go func() {
		_, err := driver.Open(context.Background(), request)
		first <- err
	}()
	<-manager.openEntered
	if _, err := driver.Open(context.Background(), request); !errors.Is(err, productruntime.ErrAmbiguousSession) {
		t.Fatalf("concurrent open = %v", err)
	}
	close(manager.openRelease)
	if err := <-first; err != nil {
		t.Fatalf("reserved open = %v", err)
	}
	manager.mu.Lock()
	openCalls := manager.openCalls
	manager.mu.Unlock()
	if openCalls != 1 {
		t.Fatalf("native server opens = %d", openCalls)
	}
}
