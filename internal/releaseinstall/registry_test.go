package releaseinstall

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

type fakeInstallStrategy struct {
	key        string
	wantArgs   []string
	state      *fakeInstallState
	discover   Discovery
	change     Change
	removeErr  error
	cleanupErr error
}

type fakeInstallState struct {
	calls             []string
	current           *NativeIdentity
	target            NativeIdentity
	returned          *NativeIdentity
	plannedAgain      *NativeIdentity
	planCalls         int
	beforeRegister    func()
	failStage         error
	failValidate      error
	failRegister      error
	failVerify        error
	failRollback      error
	failAbort         error
	removeErr         error
	cleanupErr        error
	rollbackNoRestore bool
	removeNoRestore   bool
	cleanupCalls      int
	aborts            int
}

func (strategy *fakeInstallStrategy) Key() string { return strategy.key }

func (strategy *fakeInstallStrategy) ValidateDescriptor(descriptor productcatalog.Descriptor) error {
	if !reflect.DeepEqual(descriptor.NativeRegistration.Args, strategy.wantArgs) {
		return errors.New("native registration arguments do not match strategy")
	}
	return nil
}

func (strategy *fakeInstallStrategy) Discover(context.Context, productcatalog.Descriptor) (Discovery, error) {
	if strategy.state != nil {
		strategy.state.calls = append(strategy.state.calls, "discover")
		return Discovery{Present: strategy.state.current != nil, Identity: cloneNativeIdentity(strategy.state.current)}, nil
	}
	return strategy.discover, nil
}

func (strategy *fakeInstallStrategy) Capture(context.Context, productcatalog.Descriptor, Discovery) (CapturedChange, error) {
	if strategy.state != nil {
		strategy.state.calls = append(strategy.state.calls, "capture")
		return CapturedChange{
			Baseline: Baseline{Current: cloneNativeIdentity(strategy.state.current)},
			Change:   &fakeNativeChange{state: strategy.state},
		}, nil
	}
	return CapturedChange{Baseline: Baseline{Current: cloneNativeIdentity(strategy.discover.Identity)}, Change: strategy.change}, nil
}

func (strategy *fakeInstallStrategy) Remove(_ context.Context, _ productcatalog.Descriptor, request RemovalRequest) error {
	if strategy.state != nil {
		strategy.state.calls = append(strategy.state.calls, "remove")
		if strategy.state.removeErr == nil && !strategy.state.removeNoRestore {
			strategy.state.current = cloneNativeIdentity(request.Prior)
		}
		return strategy.state.removeErr
	}
	return strategy.removeErr
}

func (strategy *fakeInstallStrategy) ReconcileCleanup(_ context.Context, _ productcatalog.Descriptor, request CleanupRequest) error {
	if strategy.state != nil {
		strategy.state.calls = append(strategy.state.calls, "cleanup")
		strategy.state.cleanupCalls++
		if !request.Planned.Equal(strategy.state.target) {
			return errors.New("cleanup planned identity mismatch")
		}
		return strategy.state.cleanupErr
	}
	return strategy.cleanupErr
}

type fakeNativeChange struct{ state *fakeInstallState }

func (change *fakeNativeChange) Stage(context.Context) error {
	change.state.calls = append(change.state.calls, "stage")
	return change.state.failStage
}

func (change *fakeNativeChange) Validate(context.Context) error {
	change.state.calls = append(change.state.calls, "validate")
	return change.state.failValidate
}

func (change *fakeNativeChange) PlannedIdentity(context.Context) (NativeIdentity, error) {
	change.state.calls = append(change.state.calls, "plan")
	change.state.planCalls++
	if change.state.planCalls > 1 && change.state.plannedAgain != nil {
		return *change.state.plannedAgain, nil
	}
	return change.state.target, nil
}

func (change *fakeNativeChange) Register(context.Context) (NativeIdentity, error) {
	change.state.calls = append(change.state.calls, "register")
	if change.state.beforeRegister != nil {
		change.state.beforeRegister()
	}
	if change.state.failRegister != nil {
		return NativeIdentity{}, change.state.failRegister
	}
	returned := change.state.target
	if change.state.returned != nil {
		returned = *change.state.returned
	}
	change.state.current = cloneNativeIdentity(&returned)
	return returned, nil
}

func (change *fakeNativeChange) Verify(context.Context, NativeIdentity) error {
	change.state.calls = append(change.state.calls, "verify")
	return change.state.failVerify
}

func (change *fakeNativeChange) Rollback(_ context.Context, baseline Baseline, _ *NativeIdentity) error {
	change.state.calls = append(change.state.calls, "rollback")
	if change.state.failRollback == nil && !change.state.rollbackNoRestore {
		change.state.current = cloneNativeIdentity(baseline.Current)
	}
	return change.state.failRollback
}

func (change *fakeNativeChange) Abort(context.Context) error {
	change.state.calls = append(change.state.calls, "abort")
	change.state.aborts++
	return change.state.failAbort
}

func TestRegistryIsExplicitCompleteAndDescriptorValidated(t *testing.T) {
	inventory := productcatalog.All()[:2]
	for index := range inventory {
		inventory[index].NativeRegistration = productcatalog.NativeRegistration{Strategy: "synthetic-" + inventory[index].ID, Args: []string{inventory[index].ID}}
	}
	strategies := []Strategy{
		&fakeInstallStrategy{key: "synthetic-claude", wantArgs: []string{"claude"}},
		&fakeInstallStrategy{key: "synthetic-codex", wantArgs: []string{"codex"}},
	}
	registry, err := NewRegistry(inventory, strategies...)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := registry.Resolve("synthetic-codex"); !ok || got.Key() != "synthetic-codex" {
		t.Fatalf("resolve = %#v, %v", got, ok)
	}

	tests := []struct {
		name       string
		inventory  []productcatalog.Descriptor
		strategies []Strategy
	}{
		{name: "missing", inventory: inventory, strategies: strategies[:1]},
		{name: "extra", inventory: inventory[:1], strategies: strategies},
		{name: "duplicate", inventory: inventory, strategies: append(strategies, strategies[0])},
		{name: "argument mismatch", inventory: inventory, strategies: []Strategy{
			&fakeInstallStrategy{key: "synthetic-claude", wantArgs: []string{"wrong"}}, strategies[1],
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRegistry(test.inventory, test.strategies...); err == nil {
				t.Fatal("invalid registry construction succeeded")
			}
		})
	}
}
