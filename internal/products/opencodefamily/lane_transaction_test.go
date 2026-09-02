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

type laneReservationManager struct {
	client *Client

	mu             sync.Mutex
	openCalls      int
	openEntered    chan struct{}
	openRelease    chan struct{}
	openSignalOnce sync.Once
}

func (manager *laneReservationManager) Open(context.Context, ServerOpenRequest) (*LiveServer, error) {
	manager.mu.Lock()
	manager.openCalls++
	entered, release := manager.openEntered, manager.openRelease
	manager.mu.Unlock()
	if entered != nil {
		manager.openSignalOnce.Do(func() { close(entered) })
		<-release
	}
	return &LiveServer{client: manager.client, closeFn: func(context.Context) error { return nil }}, nil
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
	manager := &laneReservationManager{client: client, openEntered: make(chan struct{}), openRelease: make(chan struct{})}
	driver, err := NewLaneDriver(LaneConfig{
		ProductID: "opencode", Dialect: DialectOpenCode, Generation: 7,
		Servers: manager, MapPermission: MapPermissionRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := productruntime.LaneOpenRequest{
		ProductID: "opencode", LaneID: "reserved", Name: "worker", Cwd: "/work/project", PermissionMode: permissionmode.Default,
	}
	first := make(chan error, 1)
	go func() {
		_, openErr := driver.Open(context.Background(), request)
		first <- openErr
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
