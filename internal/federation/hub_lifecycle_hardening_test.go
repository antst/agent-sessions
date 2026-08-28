//go:build linux || darwin

package federation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/antst/agent-sessions/internal/releaseinstall"
	"github.com/antst/agent-sessions/internal/servicecontrol"
)

type recordingHubReleaseTransaction struct {
	calls      []string
	recoverErr error
	installErr error
	request    releaseinstall.InstallRequest
}

func (transaction *recordingHubReleaseTransaction) Recover(context.Context) error {
	transaction.calls = append(transaction.calls, "recover")
	return transaction.recoverErr
}

func (transaction *recordingHubReleaseTransaction) Install(
	_ context.Context,
	request releaseinstall.InstallRequest,
) (releaseinstall.InstallResult, error) {
	transaction.calls = append(transaction.calls, "install")
	transaction.request = request
	return releaseinstall.InstallResult{}, transaction.installErr
}

func TestRecoverThenInstallHubSourceRecoversBeforeAnyNewSourceObservation(t *testing.T) {
	recoveryFailure := errors.New("unfinished hub transaction remains ambiguous")
	transaction := &recordingHubReleaseTransaction{recoverErr: recoveryFailure}
	err := recoverThenInstallHubSource(context.Background(), transaction, "not-an-absolute-source", "2031.1")
	if !errors.Is(err, recoveryFailure) || !reflect.DeepEqual(transaction.calls, []string{"recover"}) {
		t.Fatalf("recovery/source ordering = calls %q, error %v", transaction.calls, err)
	}

	request := hubTestRelease(t, "2031.2", "recover-first", "agent-sessions-hub")
	transaction = &recordingHubReleaseTransaction{}
	if err := recoverThenInstallHubSource(context.Background(), transaction, request.SourceRoot, request.Version); err != nil {
		t.Fatalf("recover then install hub source: %v", err)
	}
	if !reflect.DeepEqual(transaction.calls, []string{"recover", "install"}) ||
		transaction.request.ContentIdentity != request.ContentIdentity ||
		transaction.request.Version != request.Version || transaction.request.Executable != "agent-sessions-hub" {
		t.Fatalf("hub recovery/install request = calls %q request %+v", transaction.calls, transaction.request)
	}
}

func TestRecoverThenInstallHubSourceClearsInterruptedTransaction(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { makeHubTestTreeWritable(root) })
	layout, err := releaseinstall.ResolveRoleLayout(root, releaseinstall.RoleHub)
	if err != nil {
		t.Fatal(err)
	}
	service := &hubTestRoleService{}
	hooks := &hubTestRoleHooks{role: releaseinstall.RoleHub}
	engine, err := releaseinstall.NewEngine(releaseinstall.EngineOptions{Layout: layout, Service: service, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	request := hubTestRelease(t, "2031.3", "interrupted", "agent-sessions-hub")
	engine.SetCrashPoint(releaseinstall.PhasePointerCommitted)
	if _, err := engine.Install(context.Background(), request); !errors.Is(err, releaseinstall.ErrInjectedCrash) {
		t.Fatalf("create interrupted hub transaction: %v", err)
	}
	engine.SetCrashPoint("")
	if err := recoverThenInstallHubSource(context.Background(), engine, request.SourceRoot, request.Version); err != nil {
		t.Fatalf("hub CLI helper left ErrRecoveryRequired: %v", err)
	}
}

func TestInstalledHubServiceStopIsIdempotentAndReobservesRacingAbsence(t *testing.T) {
	t.Run("already absent", func(t *testing.T) {
		stops := 0
		service := &installedHubService{
			observeRuntime: func(context.Context) (hubServiceObservation, error) {
				return hubServiceObservation{}, nil
			},
			stopService: func(context.Context) error { stops++; return errors.New("must not stop absent service") },
		}
		if err := service.Stop(context.Background()); err != nil || stops != 0 {
			t.Fatalf("stop absent hub service = stops %d, error %v", stops, err)
		}
	})

	t.Run("bootout raced absence", func(t *testing.T) {
		observations := []hubServiceObservation{{loaded: true}, {}}
		service := &installedHubService{
			observeRuntime: func(context.Context) (hubServiceObservation, error) {
				observation := observations[0]
				observations = observations[1:]
				return observation, nil
			},
			stopService: func(context.Context) error { return errors.New("launchd job disappeared") },
		}
		if err := service.Stop(context.Background()); err != nil {
			t.Fatalf("idempotent racing bootout: %v", err)
		}
	})

	t.Run("ambiguous reobservation", func(t *testing.T) {
		calls := 0
		service := &installedHubService{
			observeRuntime: func(context.Context) (hubServiceObservation, error) {
				calls++
				if calls == 1 {
					return hubServiceObservation{loaded: true}, nil
				}
				return hubServiceObservation{}, errors.New("launchctl observation denied")
			},
			stopService: func(context.Context) error { return errors.New("bootout failed") },
		}
		if err := service.Stop(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "bootout failed") || !strings.Contains(err.Error(), "observation denied") {
			t.Fatalf("ambiguous stop did not fail closed: %v", err)
		}
	})
}

func TestInstalledHubServiceDarwinRestartReloadsAndReplaysFromObservedState(t *testing.T) {
	type observed struct {
		state hubServiceObservation
		err   error
	}
	tests := []struct {
		name         string
		observations []observed
		stopErr      error
		startErr     error
		wantErr      bool
		wantCalls    []string
	}{
		{
			name:         "loaded definition is booted out before current descriptor bootstrap",
			observations: []observed{{state: hubServiceObservation{loaded: true}}},
			wantCalls:    []string{"enable", "observe", "bootout", "bootstrap"},
		},
		{
			name:         "crash replay after bootout bootstraps directly",
			observations: []observed{{}},
			wantCalls:    []string{"enable", "observe", "bootstrap"},
		},
		{
			name: "failed bootout with exact absence continues bootstrap",
			observations: []observed{
				{state: hubServiceObservation{loaded: true}}, {},
			},
			stopErr:   errors.New("bootout lost its reply"),
			wantCalls: []string{"enable", "observe", "bootout", "observe", "bootstrap"},
		},
		{
			name: "failed bootout with loaded job remains ambiguous",
			observations: []observed{
				{state: hubServiceObservation{loaded: true}}, {state: hubServiceObservation{loaded: true}},
			},
			stopErr: errors.New("bootout failed"), wantErr: true,
			wantCalls: []string{"enable", "observe", "bootout", "observe"},
		},
		{
			name:         "failed initial observation remains ambiguous",
			observations: []observed{{err: errors.New("launchctl unavailable")}},
			wantErr:      true,
			wantCalls:    []string{"enable", "observe"},
		},
		{
			name: "bootstrap reply loss reobserves exact running service",
			observations: []observed{
				{}, {state: hubServiceObservation{loaded: true, state: releaseinstall.RoleServiceState{Running: true}}},
			},
			startErr:  errors.New("bootstrap lost its reply"),
			wantCalls: []string{"enable", "observe", "bootstrap", "observe"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			observations := append([]observed(nil), test.observations...)
			service := &installedHubService{
				enableService: func(context.Context) error { calls = append(calls, "enable"); return nil },
				observeRuntime: func(context.Context) (hubServiceObservation, error) {
					calls = append(calls, "observe")
					observation := observations[0]
					observations = observations[1:]
					return observation.state, observation.err
				},
				startService: func(context.Context) error { calls = append(calls, "bootstrap"); return test.startErr },
				stopService:  func(context.Context) error { calls = append(calls, "bootout"); return test.stopErr },
			}
			err := service.restartDarwin(context.Background())
			if (err != nil) != test.wantErr || !reflect.DeepEqual(calls, test.wantCalls) {
				t.Fatalf("Darwin restart = calls %q, error %v", calls, err)
			}
		})
	}
}

func TestInstalledHubServiceDarwinRestartBootstrapsReplacedListenDefinition(t *testing.T) {
	sourceRoot := writeHubLifecycleAsset(t, "plist listen=@LISTEN@")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	restore, err := installHubServiceDefinition(
		sourceRoot, "/opt/agent-sessions", t.TempDir(), "127.0.0.1:8555",
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restore() })
	target, err := hubServiceDefinitionPath()
	if err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	service := &installedHubService{
		descriptor: servicecontrol.RoleDescriptor{
			ProgramArguments: []string{"--listen", "127.0.0.1:8555"},
		},
		enableService: func(context.Context) error { calls = append(calls, "enable"); return nil },
		observeRuntime: func(context.Context) (hubServiceObservation, error) {
			calls = append(calls, "observe")
			return hubServiceObservation{loaded: true}, nil
		},
		stopService: func(context.Context) error { calls = append(calls, "bootout"); return nil },
		startService: func(context.Context) error {
			calls = append(calls, "bootstrap")
			body, readErr := os.ReadFile(target)
			if readErr != nil || !strings.Contains(string(body), "127.0.0.1:8555") {
				t.Fatalf("bootstrap did not consume replaced listen definition: %q, %v", body, readErr)
			}
			return nil
		},
		restartService: func(context.Context) error {
			t.Fatal("Darwin descriptor reload used cached kickstart arguments")
			return nil
		},
	}
	if err := service.restartDarwin(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{"enable", "observe", "bootout", "bootstrap"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("Darwin changed-listen reload calls = %q, want %q", calls, wantCalls)
	}
}

func TestInstalledHubServiceLinuxRestartReobservesACommandRace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd restart path")
	}
	recorder := &hubServiceCommandRecorder{}
	service := &installedHubService{
		runner:        recorder,
		enableService: func(context.Context) error { return nil },
		restartService: func(context.Context) error {
			return errors.New("systemctl client lost its reply")
		},
		observeRuntime: func(context.Context) (hubServiceObservation, error) {
			return hubServiceObservation{
				loaded: true, state: releaseinstall.RoleServiceState{Enabled: true, Running: true},
			}, nil
		},
	}
	if err := service.Restart(context.Background()); err != nil {
		t.Fatalf("restart command race with exact running reobservation: %v", err)
	}
	if len(recorder.commands) != 2 || recorder.commands[0].arguments[1] != "daemon-reload" ||
		recorder.commands[1].arguments[1] != "reset-failed" {
		t.Fatalf("systemd restart preparation commands = %#v", recorder.commands)
	}
}

func TestHubRecoveryWaitsForExactDelayedCandidateWithoutSecondRestart(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { makeHubTestTreeWritable(root) })
	layout, err := releaseinstall.ResolveRoleLayout(root, releaseinstall.RoleHub)
	if err != nil {
		t.Fatal(err)
	}
	request := hubTestRelease(t, "2031.4", "delayed-candidate", "agent-sessions-hub")
	wantIdentity, err := hubExecutableSHA256(filepath.Join(request.SourceRoot, "bin", "agent-sessions-hub"))
	if err != nil {
		t.Fatal(err)
	}
	reads, restarts := 0, 0
	service := &installedHubService{
		descriptor: serviceDescriptorForHubTest(request.SourceRoot),
		observeRuntime: func(context.Context) (hubServiceObservation, error) {
			return hubServiceObservation{}, nil
		},
		restartService: func(context.Context) error { restarts++; return nil },
		readStatus: func(context.Context) (HubStatusProjection, error) {
			reads++
			if reads < 3 {
				return HubStatusProjection{}, os.ErrNotExist
			}
			return HubStatusProjection{
				RuntimeVersion: request.Version, RuntimeIdentity: wantIdentity,
				Listener: "127.0.0.1:7443", ProtocolVersion: ProtocolVersion,
			}, nil
		},
		processMatches: func(HubStatusProjection) bool { return true },
		verifyTimeout:  200 * time.Millisecond,
		verifyPoll:     time.Millisecond,
	}
	hooks := &hubTestRoleHooks{role: releaseinstall.RoleHub}
	engine, err := releaseinstall.NewEngine(releaseinstall.EngineOptions{Layout: layout, Service: service, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	engine.SetCrashPoint(releaseinstall.PhasePointerCommitted)
	if _, err := engine.Install(context.Background(), request); !errors.Is(err, releaseinstall.ErrInjectedCrash) {
		t.Fatalf("create pointer-committed hub crash: %v", err)
	}
	engine.SetCrashPoint("")
	if err := engine.Recover(context.Background()); err != nil {
		t.Fatalf("recover delayed exact candidate: %v", err)
	}
	if reads != 3 || restarts != 0 {
		t.Fatalf("delayed candidate recovery = reads %d, restarts %d", reads, restarts)
	}
}

func serviceDescriptorForHubTest(root string) servicecontrol.RoleDescriptor {
	return servicecontrol.RoleDescriptor{
		Role: "hub", ServiceName: "agent-sessions-hub.service", Label: "net.antst.agent-sessions-hub",
		DefinitionPath:   filepath.Join(root, "agent-sessions-hub.service"),
		Program:          filepath.Join(root, "hub", "current", "bin", "agent-sessions-hub"),
		ProgramArguments: []string{"--listen", "127.0.0.1:7443"},
	}
}

func TestHubServiceDefinitionSourceRejectsIndirectBlockingAndSwappedFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string) *hubDefinitionFSHooks
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, asset string) *hubDefinitionFSHooks {
				external := filepath.Join(t.TempDir(), "external.service")
				if err := os.WriteFile(external, []byte("external"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(external, asset); err != nil {
					t.Fatal(err)
				}
				return nil
			},
		},
		{
			name: "fifo",
			setup: func(t *testing.T, asset string) *hubDefinitionFSHooks {
				if err := unix.Mkfifo(asset, 0o600); err != nil {
					t.Fatal(err)
				}
				return nil
			},
		},
		{
			name: "inode swap after open",
			setup: func(t *testing.T, asset string) *hubDefinitionFSHooks {
				if err := os.WriteFile(asset, []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
				replacement := asset + ".replacement"
				if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
				return &hubDefinitionFSHooks{afterSourceOpen: func() {
					if err := os.Rename(replacement, asset); err != nil {
						t.Errorf("swap source asset: %v", err)
					}
				}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sourceRoot := t.TempDir()
			asset := hubLifecycleAssetPath(sourceRoot)
			if err := os.MkdirAll(filepath.Dir(asset), 0o700); err != nil {
				t.Fatal(err)
			}
			hooks := test.setup(t, asset)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
			started := time.Now()
			if _, err := installHubServiceDefinitionWithHooks(sourceRoot, "/opt/agent-sessions", t.TempDir(), "127.0.0.1:7443", hooks); err == nil {
				t.Fatal("indirect, blocking, or swapped source asset was accepted")
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("FIFO service asset blocked the bounded nonblocking read")
			}
		})
	}
}

func TestHubServiceDefinitionRejectsTargetSymlinkAndForwardSwap(t *testing.T) {
	sourceRoot := writeHubLifecycleAsset(t, "candidate @PREFIX@ @STATE_ROOT@ @LISTEN@")
	configurationRoot := filepath.Join(t.TempDir(), "config")
	t.Setenv("XDG_CONFIG_HOME", configurationRoot)
	target, err := hubServiceDefinitionPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.service")
	if err := os.WriteFile(external, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, target); err != nil {
		t.Fatal(err)
	}
	if _, err := installHubServiceDefinition(sourceRoot, "/opt/agent-sessions", t.TempDir(), "127.0.0.1:7443"); err == nil {
		t.Fatal("target service-definition symlink was accepted")
	}
	if body, err := os.ReadFile(external); err != nil || string(body) != "external" {
		t.Fatalf("target symlink referent changed: %q, %v", body, err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("prior"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := target + ".replacement"
	if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	hooks := &hubDefinitionFSHooks{beforeMutation: func() {
		if err := os.Rename(replacement, target); err != nil {
			t.Errorf("swap target service definition: %v", err)
		}
	}}
	if _, err := installHubServiceDefinitionWithHooks(
		sourceRoot, "/opt/agent-sessions", t.TempDir(), "127.0.0.1:7443", hooks,
	); err == nil {
		t.Fatal("replacement service-definition inode was overwritten")
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "replacement" {
		t.Fatalf("replacement target changed: %q, %v", body, err)
	}
}

func TestHubServiceDefinitionRollbackIsIdentityBoundAndParentDurable(t *testing.T) {
	sourceRoot := writeHubLifecycleAsset(t, "candidate @PREFIX@ @STATE_ROOT@ @LISTEN@")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	syncs := 0
	hooks := &hubDefinitionFSHooks{syncDirectory: func(directory *os.File) error {
		syncs++
		return directory.Sync()
	}}
	restore, err := installHubServiceDefinitionWithHooks(
		sourceRoot, "/opt/agent-sessions", t.TempDir(), "127.0.0.1:7443", hooks,
	)
	if err != nil {
		t.Fatal(err)
	}
	target, err := hubServiceDefinitionPath()
	if err != nil {
		t.Fatal(err)
	}
	if syncs != 1 {
		t.Fatalf("service-definition install parent fsyncs = %d", syncs)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if syncs != 2 {
		t.Fatalf("service-definition rollback parent fsyncs = %d", syncs)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("rollback retained first-install service definition: %v", err)
	}

	restore, err = installHubServiceDefinition(sourceRoot, "/opt/agent-sessions", t.TempDir(), "127.0.0.1:7443")
	if err != nil {
		t.Fatal(err)
	}
	replacement := target + ".replacement"
	if err := os.WriteFile(replacement, []byte("replacement-after-install"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, target); err != nil {
		t.Fatal(err)
	}
	if err := restore(); err == nil {
		t.Fatal("rollback removed a replacement service-definition inode")
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "replacement-after-install" {
		t.Fatalf("rollback changed replacement service definition: %q, %v", body, err)
	}
}

func hubLifecycleAssetPath(sourceRoot string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(sourceRoot, "deploy", "agent-sessions-hub", "launchd", "net.antst.agent-sessions-hub.plist")
	}
	return filepath.Join(sourceRoot, "deploy", "agent-sessions-hub", "systemd", "user", "agent-sessions-hub.service")
}

func writeHubLifecycleAsset(t *testing.T, body string) string {
	t.Helper()
	sourceRoot := t.TempDir()
	home := filepath.Join(sourceRoot, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	asset := hubLifecycleAssetPath(sourceRoot)
	if err := os.MkdirAll(filepath.Dir(asset), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return sourceRoot
}
