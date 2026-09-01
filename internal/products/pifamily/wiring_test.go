package pifamily_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/antst/agent-sessions/internal/component"
	"github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/productruntime"
	"github.com/antst/agent-sessions/internal/products/omp"
	"github.com/antst/agent-sessions/internal/products/pi"
	"github.com/antst/agent-sessions/internal/products/pifamily"
)

type wiringSender struct{}

func (wiringSender) Send(string, component.FrameType, string, any) error { return nil }
func (wiringSender) RenameSession(_ context.Context, _, _ string, request component.SessionRenameRequest) (component.SessionRename, error) {
	return component.SessionRename{
		NativeSessionID: request.NativeSessionID, NativeName: request.RequestedName, ProductEventSeq: 2,
	}, nil
}

type wiringBindings struct{ binding component.BindingView }

func (source wiringBindings) Bindings() []component.BindingView {
	return []component.BindingView{source.binding}
}

type wiringTools struct{}

func (wiringTools) HandleParentTool(context.Context, pifamily.ParentToolCall) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":true}`), nil
}

type wiringProcesses struct{}

func (wiringProcesses) CaptureIdentity(context.Context, int) (procinfo.Identity, error) {
	return procinfo.Identity{}, errors.New("unused")
}
func (wiringProcesses) ObserveIdentity(context.Context, procinfo.Identity) (procinfo.IdentityObservation, error) {
	return procinfo.IdentityObservation{}, errors.New("unused")
}
func (wiringProcesses) Executable(context.Context, procinfo.Identity) (string, error) {
	return "", errors.New("unused")
}
func (wiringProcesses) DescendsFrom(context.Context, procinfo.Identity, procinfo.Identity, int) (bool, error) {
	return false, errors.New("unused")
}

type wiringRPCFactory struct{}

func (wiringRPCFactory) StartRPC(context.Context, productruntime.NativeCommand) (pifamily.RPCProcess, error) {
	return nil, errors.New("unused")
}

type wiringReceipts struct{}

func (wiringReceipts) OpenReceipt(string) (io.ReadCloser, int64, [32]byte, error) {
	return nil, 0, [32]byte{}, errors.New("unused")
}

func TestProductRuntimeRequiresAndUsesExactPrewiredObserver(t *testing.T) {
	if _, err := pi.NewRuntime(productcatalog.Descriptor{ID: pi.ProductID}, pi.Config{}); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("Pi nil observer error = %v", err)
	}
	if _, err := omp.NewRuntime(productcatalog.Descriptor{ID: omp.ProductID}, omp.Config{}); !errors.Is(err, productruntime.ErrProtocol) {
		t.Fatalf("OMP nil observer error = %v", err)
	}

	for _, productID := range []string{pi.ProductID, omp.ProductID} {
		t.Run(productID, func(t *testing.T) {
			binding := component.BindingView{
				BindingID: "binding-" + productID, AttachmentID: "attachment-" + productID,
				ProductID: productID, Generation: 1,
			}
			sender, bindings := wiringSender{}, wiringBindings{binding: binding}
			deps := productruntime.HostDeps{Generation: 1, Receipts: wiringReceipts{}, Processes: wiringProcesses{}}
			var observer *pifamily.ComponentRuntime
			var runtimeProduct productruntime.RuntimeProduct
			var err error
			if productID == pi.ProductID {
				config := pi.Config{
					Deps: deps, ExtensionPath: "/managed/pi/agent-sessions.mjs", ComponentSocket: "/runtime/component.sock",
					Processes: wiringRPCFactory{}, Sender: sender, Bindings: bindings, ParentTools: wiringTools{},
				}
				observer, err = pi.NewComponentObserver(config)
				if err == nil {
					config.Component = observer
					runtimeProduct, err = pi.NewRuntime(productcatalog.Descriptor{ID: productID}, config)
				}
			} else {
				config := omp.Config{
					Deps: deps, ExtensionPath: "/managed/omp/agent-sessions.mjs", ComponentSocket: "/runtime/component.sock",
					Processes: wiringRPCFactory{}, Sender: sender, Bindings: bindings, ParentTools: wiringTools{},
				}
				observer, err = omp.NewComponentObserver(config)
				if err == nil {
					config.Component = observer
					runtimeProduct, err = omp.NewRuntime(productcatalog.Descriptor{ID: productID}, config)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if runtimeProduct.NativeTitle == nil {
				t.Fatal("Pi-family runtime omitted its live native-title projector")
			}
			announce, err := component.NewFrame(component.TypeSessionAnnounce, "announce", 1, component.SessionAnnounce{
				BindingID: binding.BindingID, NativeSessionID: "native-" + productID,
				Cwd: "/work", NativeName: "", ProductEventSeq: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := observer.HandleComponentFrame(context.Background(), binding, announce); err != nil {
				t.Fatal(err)
			}
			projected, err := runtimeProduct.NativeTitle.ProjectNativeTitle(context.Background(), daemon.ManagedAttachment{
				ID: binding.AttachmentID, Product: productID, NativeSessionID: "native-" + productID,
				State: "attached", DaemonGeneration: binding.Generation,
			})
			if err != nil || projected != (productruntime.NativeTitleProjection{NativeSessionID: "native-" + productID, Title: ""}) {
				t.Fatalf("prewired native title = %+v, %v", projected, err)
			}
			renamed, err := runtimeProduct.Peer.Rename(context.Background(), daemon.ManagedAttachment{
				ID: binding.AttachmentID, Product: productID, NativeSessionID: "native-" + productID, State: "attached",
			}, "wired title")
			if err != nil || !renamed.NativeConfirmed || renamed.Applied != "wired title" {
				t.Fatalf("prewired observer rename = %+v, %v", renamed, err)
			}
		})
	}
}
