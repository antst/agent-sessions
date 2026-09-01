package releaseinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type recordingService struct {
	validated []string
	activated []string
	removed   int
	validate  error
	activate  error
	remove    error
}

func (service *recordingService) Validate(_ context.Context, releaseRoot string) error {
	service.validated = append(service.validated, releaseRoot)
	return service.validate
}

func (service *recordingService) Activate(_ context.Context, releaseRoot string) error {
	service.activated = append(service.activated, releaseRoot)
	return service.activate
}

func (service *recordingService) Remove(context.Context) error {
	service.removed++
	return service.remove
}

func TestInstallValidatesStagedReleaseBeforeCommit(t *testing.T) {
	prefix := t.TempDir()
	service := &recordingService{validate: errors.New("invalid staged service")}
	transaction := Transaction{Prefix: prefix, Service: service}
	source := releaseFixture(t, HostRole, "candidate")

	err := transaction.Install(context.Background(), InstallRequest{
		Role: HostRole, Version: "0.3.0", Platform: "linux-x64", SourceDir: source,
	})
	if err == nil {
		t.Fatal("invalid staged release was committed")
	}
	if len(service.validated) != 1 || len(service.activated) != 0 {
		t.Fatalf("service calls = validate %d activate %d", len(service.validated), len(service.activated))
	}
	assertAbsent(t, filepath.Join(prefix, "libexec", "agent-sessions", "host", "current"))
	assertAbsent(t, filepath.Join(prefix, "bin", "agent-sessions"))
}

func TestInstallRejectsIndirectReleasePayloadBeforeCommit(t *testing.T) {
	prefix := t.TempDir()
	service := &recordingService{}
	transaction := Transaction{Prefix: prefix, Service: service}
	source := releaseFixture(t, HostRole, "candidate")
	if err := os.Symlink("payload", filepath.Join(source, "indirect")); err != nil {
		t.Fatal(err)
	}

	err := transaction.Install(context.Background(), InstallRequest{
		Role: HostRole, Version: "0.3.0", Platform: "linux-x64", SourceDir: source,
	})
	if err == nil || len(service.validated) != 0 || len(service.activated) != 0 {
		t.Fatalf("indirect payload was accepted: err=%v validate=%d activate=%d", err, len(service.validated), len(service.activated))
	}
	assertAbsent(t, filepath.Join(prefix, "libexec", "agent-sessions", "host", "current"))
}

func TestInstallRollsBackPartialCommitAndPreservesPriorSelection(t *testing.T) {
	prefix := t.TempDir()
	service := &recordingService{}
	transaction := Transaction{Prefix: prefix, Service: service}
	installFixture(t, transaction, HostRole, "prior")
	prior := selectedRelease(t, prefix, HostRole)

	transaction.AfterCommit = func(string) error { return errors.New("injected partial commit failure") }
	err := transaction.Install(context.Background(), InstallRequest{
		Role: HostRole, Version: "0.3.1", Platform: "linux-x64",
		SourceDir: releaseFixture(t, HostRole, "candidate"),
	})
	if err == nil {
		t.Fatal("partial commit failure was ignored")
	}
	if got := selectedRelease(t, prefix, HostRole); got != prior {
		t.Fatalf("selected release = %q, want prior %q", got, prior)
	}
	assertFileBody(t, filepath.Join(prior, "bin", "agent-sessions"), "prior")
	assertAbsent(t, filepath.Join(prefix, "libexec", "agent-sessions", "host", "releases", "0.3.1"))
	if len(service.activated) != 1 {
		t.Fatalf("partial commit activated service %d times, want only prior install", len(service.activated))
	}
}

func TestFailedSameVersionUpdateRestoresExactPriorInstall(t *testing.T) {
	prefix := t.TempDir()
	service := &recordingService{}
	transaction := Transaction{Prefix: prefix, Service: service}
	installFixture(t, transaction, HubRole, "prior-hub")
	prior := selectedRelease(t, prefix, HubRole)
	service.activate = errors.New("service rejected candidate")

	err := transaction.Install(context.Background(), InstallRequest{
		Role: HubRole, Version: "0.3.0", Platform: "linux-x64",
		SourceDir: releaseFixture(t, HubRole, "candidate-hub"),
	})
	if err == nil {
		t.Fatal("failed same-version update succeeded")
	}
	if got := selectedRelease(t, prefix, HubRole); got != prior {
		t.Fatalf("selected release = %q, want %q", got, prior)
	}
	assertFileBody(t, filepath.Join(prior, "bin", "agent-sessions-hub"), "prior-hub")
	if len(service.activated) != 2 {
		t.Fatalf("activate calls = %d, want one per attempted install", len(service.activated))
	}
}

func TestSuccessfulInstallRestartsServiceExactlyOnce(t *testing.T) {
	prefix := t.TempDir()
	service := &recordingService{}
	transaction := Transaction{Prefix: prefix, Service: service}
	installFixture(t, transaction, HostRole, "candidate")

	if len(service.validated) != 1 || len(service.activated) != 1 {
		t.Fatalf("service calls = validate %d activate %d, want 1/1", len(service.validated), len(service.activated))
	}
	selected := selectedRelease(t, prefix, HostRole)
	if service.activated[0] != selected {
		t.Fatalf("activated release = %q, selected = %q", service.activated[0], selected)
	}
	alias := filepath.Join(prefix, "bin", "agent-sessions")
	if target, err := filepath.EvalSymlinks(alias); err != nil || target != filepath.Join(selected, "bin", "agent-sessions") {
		t.Fatalf("alias target = %q, %v", target, err)
	}
}

func TestRemovalIsRoleScopedAndPreservesState(t *testing.T) {
	prefix := t.TempDir()
	hostService := &recordingService{}
	hubService := &recordingService{}
	installFixture(t, Transaction{Prefix: prefix, Service: hostService}, HostRole, "host")
	installFixture(t, Transaction{Prefix: prefix, Service: hubService}, HubRole, "hub")
	stateCanary := filepath.Join(prefix, "state-canary")
	if err := os.WriteFile(stateCanary, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (Transaction{Prefix: prefix, Service: hostService}).Remove(context.Background(), HostRole); err != nil {
		t.Fatal(err)
	}
	if hostService.removed != 1 {
		t.Fatalf("host service removal calls = %d, want 1", hostService.removed)
	}
	assertAbsent(t, filepath.Join(prefix, "libexec", "agent-sessions", "host"))
	assertAbsent(t, filepath.Join(prefix, "bin", "agent-sessions"))
	if _, err := os.Stat(filepath.Join(prefix, "bin", "agent-sessions-hub")); err != nil {
		t.Fatalf("hub alias was not preserved: %v", err)
	}
	assertFileBody(t, stateCanary, "preserved")
}

func TestFailedServiceRemovalPreservesInstalledRole(t *testing.T) {
	prefix := t.TempDir()
	service := &recordingService{}
	transaction := Transaction{Prefix: prefix, Service: service}
	installFixture(t, transaction, HostRole, "host")
	prior := selectedRelease(t, prefix, HostRole)
	service.remove = errors.New("service still busy")

	if err := transaction.Remove(context.Background(), HostRole); err == nil {
		t.Fatal("failed service removal mutated the installation")
	}
	if got := selectedRelease(t, prefix, HostRole); got != prior {
		t.Fatalf("selected release = %q, want %q", got, prior)
	}
	assertFileBody(t, filepath.Join(prior, "bin", "agent-sessions"), "host")
}

func releaseFixture(t *testing.T, role Role, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", role.BinaryName()), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func installFixture(t *testing.T, transaction Transaction, role Role, body string) {
	t.Helper()
	if err := transaction.Install(context.Background(), InstallRequest{
		Role: role, Version: "0.3.0", Platform: "linux-x64", SourceDir: releaseFixture(t, role, body),
	}); err != nil {
		t.Fatal(err)
	}
}

func selectedRelease(t *testing.T, prefix string, role Role) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(filepath.Join(prefix, "libexec", "agent-sessions", string(role), "current"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists or cannot be checked: %v", path, err)
	}
}

func assertFileBody(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("%s = %q, want %q", path, body, want)
	}
}
