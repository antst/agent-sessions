package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/productruntime"
)

type inertComponents struct{}

func (inertComponents) LookupComponent(context.Context, string, string) (productruntime.ComponentSessionView, error) {
	return productruntime.ComponentSessionView{}, productruntime.ErrStale
}

type inertProcesses struct{}

func (inertProcesses) CaptureIdentity(context.Context, int) (procinfo.Identity, error) {
	return procinfo.Identity{}, productruntime.ErrStale
}
func (inertProcesses) ObserveIdentity(context.Context, procinfo.Identity) (procinfo.IdentityObservation, error) {
	return procinfo.IdentityObservation{}, productruntime.ErrStale
}
func (inertProcesses) Executable(context.Context, procinfo.Identity) (string, error) {
	return "", productruntime.ErrStale
}
func (inertProcesses) DescendsFrom(context.Context, procinfo.Identity, procinfo.Identity, int) (bool, error) {
	return false, productruntime.ErrStale
}

type inertGateway struct{}

func (inertGateway) Deliver(context.Context, string, string, productruntime.DeliveryRequest) (productruntime.NativeAcceptance, error) {
	return productruntime.NativeAcceptance{}, productruntime.ErrStale
}
func (inertGateway) Rename(context.Context, string, string, string) (productruntime.NativeName, error) {
	return productruntime.NativeName{}, productruntime.ErrStale
}

func TestOpenCodeBuildLaunchIsPluginTUIWithLiveComponentIdentity(t *testing.T) {
	driver, err := NewPeerDriver(Config{
		Deps:    productruntime.HostDeps{Generation: 1, Components: inertComponents{}, Processes: inertProcesses{}},
		Gateway: inertGateway{}, Executable: "/native/opencode",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := productruntime.PeerLaunchRequest{
		ProductID: ProductID, AttachmentID: "attachment-one", Cwd: "/work/project",
		Args: []string{"--continue"}, Env: []productruntime.EnvVar{{Name: "TERM", Value: "xterm"}},
	}
	command, err := driver.BuildLaunch(context.Background(), request)
	if err != nil || command.Path != "/native/opencode" || len(command.Args) != 1 || command.Args[0] != "--continue" || command.Cwd != request.Cwd {
		t.Fatalf("command = %#v, %v", command, err)
	}
	if len(command.SensitiveEnv) != 0 {
		t.Fatalf("sensitive env = %#v", command.SensitiveEnv)
	}
	componentRevision := ""
	for _, variable := range command.Env {
		if variable.Name == "AGENT_SESSIONS_COMPONENT_VERSION" {
			componentRevision = variable.Value
		}
	}
	if componentRevision != "agent-sessions.component.v1-r1" {
		t.Fatalf("component bootstrap revision = %q", componentRevision)
	}
	if _, err := json.Marshal(command); err == nil {
		t.Fatal("native command serialized")
	}
	for _, argument := range [][]string{{"serve"}, {"--port=1234"}, {"run"}, {"--mini"}} {
		request.Args = argument
		if _, err := driver.BuildLaunch(context.Background(), request); !errors.Is(err, productruntime.ErrUnsupportedPolicy) {
			t.Fatalf("unsafe peer args %v = %v", argument, err)
		}
	}
}
