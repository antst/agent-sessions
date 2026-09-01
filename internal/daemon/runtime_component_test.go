package daemon

import (
	"context"
	"testing"

	"github.com/antst/agent-sessions/internal/procinfo"
)

func TestRuntimeGenerationCommitAtomicallyRetiresComponentAuthority(t *testing.T) {
	for _, state := range []BindingState{BindingBinding, BindingReady} {
		t.Run(string(state), func(t *testing.T) {
			root := shortDaemonTestRoot(t)
			store, err := OpenState(root, defaultRuntimeStateBytes)
			if err != nil {
				t.Fatal(err)
			}
			identity := procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}
			catalog := Catalog{
				Host: HostRuntime{User: "1000", Host: "host", Generation: 4, ServiceState: "running"},
				Attachments: map[string]ManagedAttachment{
					"attachment": {
						ID: "attachment", CapabilityHash: CapabilityDigest("secret"), Product: "codex",
						ProfileIdentity: "profile", NativeSessionID: "native", CatalogRevision: 7,
						ExpectedEvidence: NativeEvidence{Process: identity}, Evidence: NativeEvidence{Process: identity},
						DaemonGeneration: 4, State: "attached",
					},
				},
				ComponentBindings: map[string]ComponentBinding{
					"binding": {
						Schema: ComponentBindingRecordSchema, BindingID: "binding", AttachmentID: "attachment",
						ProcessIdentity: identity, BootstrapRevision: 7, Generation: 4, State: BindingBinding,
					},
				},
				ComponentSessions: map[string]ComponentSession{
					"attachment": {
						Schema: ComponentSessionRecordSchema, AttachmentID: "attachment", BindingID: "binding",
						NativeSessionID: "native", State: ComponentSessionAnnounced, LastEventSeq: 1,
					},
				},
			}
			committed, err := store.Commit(0, catalog)
			if err != nil {
				t.Fatal(err)
			}
			catalog = committed.Catalog
			binding := catalog.ComponentBindings["binding"]
			binding.State, binding.LastInboundSeq = state, 2
			catalog.ComponentBindings["binding"] = binding
			session := catalog.ComponentSessions["attachment"]
			session.State, session.LastEventSeq = ComponentSessionBusy, 3
			catalog.ComponentSessions["attachment"] = session
			if _, err := store.Commit(committed.Revision, catalog); err != nil {
				t.Fatal(err)
			}

			var initialized bool
			runtime, err := StartRuntime(context.Background(), RuntimeConfig{
				StateRoot: root,
				Initialize: func(runtime *Runtime) error {
					snapshot, readErr := runtime.State().Read()
					if readErr != nil {
						return readErr
					}
					binding := snapshot.Catalog.ComponentBindings["binding"]
					session := snapshot.Catalog.ComponentSessions["attachment"]
					if snapshot.Catalog.Host.Generation != 5 || binding.Generation != 4 ||
						binding.State != BindingRetiring || session.State != ComponentSessionClosed {
						t.Fatalf("startup component sweep = host=%d binding=%+v session=%+v",
							snapshot.Catalog.Host.Generation, binding, session)
					}
					initialized = true
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			if !initialized || runtime.Generation() != 5 {
				t.Fatalf("runtime initialized=%v generation=%d", initialized, runtime.Generation())
			}
		})
	}
}

func TestRuntimeGenerationSweepLeavesTerminalComponentRowsExact(t *testing.T) {
	identity := procinfo.Identity{PID: 42, Start: "start", StrongStart: "strong"}
	catalog := Catalog{
		ComponentBindings: map[string]ComponentBinding{
			"retiring": {BindingID: "retiring", State: BindingRetiring, Generation: 2, ProcessIdentity: identity},
			"closed":   {BindingID: "closed", State: BindingClosed, Generation: 1, ProcessIdentity: identity},
		},
		ComponentSessions: map[string]ComponentSession{
			"closed": {AttachmentID: "closed", BindingID: "closed", State: ComponentSessionClosed},
		},
	}
	retireStaleComponentAuthority(&catalog)
	if catalog.ComponentBindings["retiring"].State != BindingRetiring ||
		catalog.ComponentBindings["closed"].State != BindingClosed ||
		catalog.ComponentSessions["closed"].State != ComponentSessionClosed {
		t.Fatalf("terminal rows changed: %+v / %+v", catalog.ComponentBindings, catalog.ComponentSessions)
	}
}
