package daemon

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/antst/agent-sessions/internal/productcatalog"
	"github.com/antst/agent-sessions/internal/releaseinstall"
)

func TestHostInstallHooksPrepareAndCommitAllFourConnectors(t *testing.T) {
	recorder := &connectorRecorder{}
	drivers := connectorDriversForCatalog(recorder)
	hooks, err := NewHostInstallHooks(drivers)
	if err != nil {
		t.Fatal(err)
	}
	request := releaseinstall.InstallRequest{SourceRoot: "/staged/host-release"}
	if err := hooks.Prepare(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	wantPrepared := make([]string, 0, 8)
	wantPrepared = append(wantPrepared, "prepare:codex", "prepare:claude", "prepare:grok", "prepare:qwen")
	if !reflect.DeepEqual(recorder.calls, wantPrepared) {
		t.Fatalf("connector preparation = %q, want %q", recorder.calls, wantPrepared)
	}
	if err := hooks.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantCommitted := append(append([]string(nil), wantPrepared...), "commit:codex", "commit:claude", "commit:grok", "commit:qwen")
	if !reflect.DeepEqual(recorder.calls, wantCommitted) {
		t.Fatalf("connector commit = %q, want %q", recorder.calls, wantCommitted)
	}
}

func TestHostInstallHooksRollbackEveryPreparedConnectorInReverseOrder(t *testing.T) {
	recorder := &connectorRecorder{prepareFailure: "grok"}
	hooks, err := NewHostInstallHooks(connectorDriversForCatalog(recorder))
	if err != nil {
		t.Fatal(err)
	}
	if err := hooks.Prepare(context.Background(), releaseinstall.InstallRequest{SourceRoot: "/staged/host-release"}); err == nil {
		t.Fatal("connector preparation failure was ignored")
	}
	want := []string{"prepare:codex", "prepare:claude", "prepare:grok", "rollback:claude", "rollback:codex"}
	if !reflect.DeepEqual(recorder.calls, want) {
		t.Fatalf("prepare rollback = %q, want %q", recorder.calls, want)
	}
}

func TestHostInstallHooksCommitFailureRestoresExactPriorConnectorState(t *testing.T) {
	recorder := &connectorRecorder{commitFailure: "grok"}
	hooks, err := NewHostInstallHooks(connectorDriversForCatalog(recorder))
	if err != nil {
		t.Fatal(err)
	}
	if err := hooks.Prepare(context.Background(), releaseinstall.InstallRequest{SourceRoot: "/staged/host-release"}); err != nil {
		t.Fatal(err)
	}
	if err := hooks.Commit(context.Background()); err == nil {
		t.Fatal("connector commit failure was ignored")
	}
	want := []string{
		"prepare:codex", "prepare:claude", "prepare:grok", "prepare:qwen",
		"commit:codex", "commit:claude", "commit:grok",
		"rollback:qwen", "rollback:grok", "rollback:claude", "rollback:codex",
	}
	if !reflect.DeepEqual(recorder.calls, want) {
		t.Fatalf("commit rollback = %q, want %q", recorder.calls, want)
	}
	for product, state := range recorder.states {
		if state != "prior:"+product {
			t.Errorf("%s state = %q, want exact prior state", product, state)
		}
	}
}

func TestHostRemovalUsesEveryProductSupportedConnectorRemoval(t *testing.T) {
	recorder := &connectorRecorder{}
	hooks, err := NewHostInstallHooks(connectorDriversForCatalog(recorder))
	if err != nil {
		t.Fatal(err)
	}
	if err := hooks.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"remove:codex", "remove:claude", "remove:grok", "remove:qwen"}
	if !reflect.DeepEqual(recorder.calls, want) {
		t.Fatalf("connector removal = %q, want %q", recorder.calls, want)
	}
}

func TestHubRoleHasNoHostConnectorHooks(t *testing.T) {
	if hooks := HubInstallHooks(); hooks != nil {
		t.Fatalf("hub role unexpectedly owns host connector hooks: %#v", hooks)
	}
}

type connectorRecorder struct {
	calls          []string
	states         map[string]string
	prepareFailure string
	commitFailure  string
}

func (r *connectorRecorder) record(value string) {
	r.calls = append(r.calls, value)
	if r.states == nil {
		r.states = map[string]string{}
	}
}

type fakeConnectorDriver struct {
	product  string
	recorder *connectorRecorder
}

func (d fakeConnectorDriver) Prepare(_ context.Context, request ConnectorRequest) (ConnectorMutation, error) {
	d.recorder.record("prepare:" + d.product)
	if request.Product != d.product || request.SourceRoot == "" || request.Descriptor != productcatalogProduct(d.product).Connector {
		return nil, errors.New("connector request did not carry its authoritative product payload")
	}
	if d.recorder.prepareFailure == d.product {
		return nil, errors.New("injected prepare failure")
	}
	d.recorder.states[d.product] = "prepared:" + d.product
	return &fakeConnectorMutation{product: d.product, recorder: d.recorder}, nil
}

func (d fakeConnectorDriver) Remove(context.Context) error {
	d.recorder.record("remove:" + d.product)
	return nil
}

type fakeConnectorMutation struct {
	product  string
	recorder *connectorRecorder
}

func (m *fakeConnectorMutation) Commit(context.Context) error {
	m.recorder.record("commit:" + m.product)
	if m.recorder.commitFailure == m.product {
		return errors.New("injected commit failure")
	}
	m.recorder.states[m.product] = "current:" + m.product
	return nil
}

func (m *fakeConnectorMutation) Rollback(context.Context) error {
	m.recorder.record("rollback:" + m.product)
	m.recorder.states[m.product] = "prior:" + m.product
	return nil
}

func connectorDriversForCatalog(recorder *connectorRecorder) map[string]ConnectorDriver {
	drivers := map[string]ConnectorDriver{}
	for _, product := range productcatalog.Catalog().Products {
		drivers[product.ID] = fakeConnectorDriver{product: product.ID, recorder: recorder}
	}
	return drivers
}

func productcatalogProduct(id string) productcatalog.ProductDescriptor {
	product, _ := productcatalog.ProductByID(id)
	return product
}
