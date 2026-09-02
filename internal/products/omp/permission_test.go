package omp

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/antst/agent-sessions/internal/permissionmode"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/products/pifamily"
)

func TestMapPermissionFailsClosed(t *testing.T) {
	if _, err := MapPermission(permissionmode.Default); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("unmediated default mapping error = %v", err)
	}
	bypass, err := MapPermission(permissionmode.BypassPermissions)
	if err != nil || bypass.Name != "yolo" || !reflect.DeepEqual(bypass.Args, []string{"--approval-mode=yolo"}) {
		t.Fatalf("bypass mapping = %#v, %v", bypass, err)
	}
	if _, err := MapPermission(permissionmode.Mode("write")); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
		t.Fatalf("unknown policy error = %v", err)
	}
}

type rejectingProcessFactory struct{ started bool }

func (factory *rejectingProcessFactory) StartRPC(context.Context, productruntime.NativeCommand) (pifamily.RPCProcess, error) {
	factory.started = true
	return nil, errors.New("unexpected RPC start")
}

func TestDefaultApprovalPolicyRejectsBeforeStartingUnattendedRPC(t *testing.T) {
	quirks, _ := pifamily.QuirksFor(ProductID)
	factory := &rejectingProcessFactory{}
	driver, err := pifamily.NewLaneDriver(pifamily.LaneConfig{
		Quirks: quirks, Generation: 1, Processes: factory, MapPermission: MapPermission,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = driver.Open(context.Background(), productruntime.LaneOpenRequest{
		ProductID: ProductID, LaneID: "default-rejected", Name: "default rejected", Cwd: "/work", PermissionMode: permissionmode.Default,
	})
	if !errors.Is(err, productruntime.ErrUnsupportedPolicy) || factory.started {
		t.Fatalf("default open = %v, process started = %t", err, factory.started)
	}
}
