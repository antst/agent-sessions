package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func TestHostSurfaceServiceSnapshotRejectsPathSwapAfterDescriptorOpen(t *testing.T) {
	root := t.TempDir()
	servicePath := filepath.Join(root, "agent-sessions.service")
	original := []byte("original service")
	outsidePath := filepath.Join(root, "outside.service")
	outside := []byte("outside service")
	if err := os.WriteFile(servicePath, original, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsidePath, outside, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := snapshotHostSurfaceService(servicePath, func() {
		if renameErr := os.Rename(servicePath, servicePath+".opened"); renameErr != nil {
			t.Fatal(renameErr)
		}
		if symlinkErr := os.Symlink(outsidePath, servicePath); symlinkErr != nil {
			t.Fatal(symlinkErr)
		}
	})
	if err == nil {
		t.Fatal("service provenance accepted a pathname swapped after descriptor open")
	}
	if body, readErr := os.ReadFile(outsidePath); readErr != nil || string(body) != string(outside) {
		t.Fatalf("rejected service swap mutated outside file: %q, %v", body, readErr)
	}
}

func TestHostSurfaceSnapshotsRejectIndirectAndNonRegularObjects(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realParent, "service"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("prior", filepath.Join(realParent, "alias")); err != nil {
		t.Fatal(err)
	}
	indirectParent := filepath.Join(root, "indirect")
	if err := os.Symlink(realParent, indirectParent); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotHostSurfaceService(filepath.Join(indirectParent, "service"), nil); err == nil {
		t.Fatal("service snapshot followed an intermediate parent symlink")
	}
	if _, err := snapshotHostSurfaceAlias(filepath.Join(indirectParent, "alias"), nil); err == nil {
		t.Fatal("alias snapshot followed an intermediate parent symlink")
	}
	serviceLink := filepath.Join(root, "service-link")
	if err := os.Symlink(filepath.Join(realParent, "service"), serviceLink); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotHostSurfaceService(serviceLink, nil); err == nil {
		t.Fatal("service snapshot followed a final symlink")
	}
	fifo := filepath.Join(root, "service-fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotHostSurfaceService(fifo, nil); err == nil {
		t.Fatal("service snapshot accepted a FIFO")
	}
}

func TestHostSurfaceAliasSnapshotRejectsSwapDuringReadlink(t *testing.T) {
	root := t.TempDir()
	aliasPath := filepath.Join(root, "agent-sessions")
	if err := os.Symlink("prior", aliasPath); err != nil {
		t.Fatal(err)
	}
	_, err := snapshotHostSurfaceAlias(aliasPath, func() {
		if removeErr := os.Remove(aliasPath); removeErr != nil {
			t.Fatal(removeErr)
		}
		if symlinkErr := os.Symlink("replacement", aliasPath); symlinkErr != nil {
			t.Fatal(symlinkErr)
		}
	})
	if err == nil {
		t.Fatal("alias provenance accepted two identities across stat and readlink")
	}
}

func TestHostSurfaceRestoresAreAtomicAndReportDirectorySyncFailure(t *testing.T) {
	syncFailure := errors.New("injected parent sync failure")
	t.Run("service", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "agent-sessions.service")
		if err := os.WriteFile(path, []byte("candidate"), 0o600); err != nil {
			t.Fatal(err)
		}
		record := hostSurfaceServiceRollback{Path: path, Present: true, Mode: 0o640, Body: []byte("prior")}
		calls := 0
		err := restoreHostSurfaceService(record, &hostSurfaceMutationHooks{syncDirectory: func(*os.File) error {
			calls++
			return syncFailure
		}})
		if !errors.Is(err, syncFailure) || calls != 1 {
			t.Fatalf("service restore sync fault = %v, calls = %d", err, calls)
		}
		if body, readErr := os.ReadFile(path); readErr != nil || string(body) != "prior" {
			t.Fatalf("service replacement was not atomic before reported sync fault: %q, %v", body, readErr)
		}
		if err := restoreHostSurfaceService(record, nil); err != nil {
			t.Fatalf("service restore was not safely retryable: %v", err)
		}
	})

	t.Run("alias", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "agent-sessions")
		if err := os.Symlink("candidate", path); err != nil {
			t.Fatal(err)
		}
		record := hostSurfaceAliasRollback{Path: path, Present: true, Target: "prior"}
		calls := 0
		err := restoreHostSurfaceAlias(record, &hostSurfaceMutationHooks{syncDirectory: func(*os.File) error {
			calls++
			return syncFailure
		}})
		if !errors.Is(err, syncFailure) || calls != 1 {
			t.Fatalf("alias restore sync fault = %v, calls = %d", err, calls)
		}
		if target, readErr := os.Readlink(path); readErr != nil || target != "prior" {
			t.Fatalf("alias replacement was not atomic before reported sync fault: %q, %v", target, readErr)
		}
		if err := restoreHostSurfaceAlias(record, nil); err != nil {
			t.Fatalf("alias restore was not safely retryable: %v", err)
		}
	})

	t.Run("absent alias retry re-syncs removal", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "agent-sessions")
		if err := os.Symlink("candidate", path); err != nil {
			t.Fatal(err)
		}
		record := hostSurfaceAliasRollback{Path: path}
		if err := restoreHostSurfaceAlias(record, &hostSurfaceMutationHooks{
			syncDirectory: func(*os.File) error { return syncFailure },
		}); !errors.Is(err, syncFailure) {
			t.Fatalf("alias removal sync fault = %v", err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("alias removal was not applied before sync fault: %v", err)
		}
		calls := 0
		if err := restoreHostSurfaceAlias(record, &hostSurfaceMutationHooks{
			syncDirectory: func(directory *os.File) error {
				calls++
				return directory.Sync()
			},
		}); err != nil || calls != 1 {
			t.Fatalf("absent alias retry did not re-sync removal: %v, calls = %d", err, calls)
		}
	})
}

func TestHostSurfaceServiceRestoreRefusesSymlinkWithoutTouchingTarget(t *testing.T) {
	root := t.TempDir()
	outsidePath := filepath.Join(root, "outside")
	outside := []byte("outside")
	if err := os.WriteFile(outsidePath, outside, 0o600); err != nil {
		t.Fatal(err)
	}
	servicePath := filepath.Join(root, "agent-sessions.service")
	if err := os.Symlink(outsidePath, servicePath); err != nil {
		t.Fatal(err)
	}
	record := hostSurfaceServiceRollback{Path: servicePath, Present: true, Mode: 0o600, Body: []byte("prior")}
	if err := restoreHostSurfaceService(record, nil); err == nil {
		t.Fatal("service restore replaced an unexpected symlink")
	}
	if body, err := os.ReadFile(outsidePath); err != nil || string(body) != string(outside) {
		t.Fatalf("rejected service symlink mutated its target: %q, %v", body, err)
	}
	if target, err := os.Readlink(servicePath); err != nil || target != outsidePath {
		t.Fatalf("rejected service symlink itself changed: %q, %v", target, err)
	}
}

func TestHostSurfaceRestoresRejectIntermediateParentSymlink(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	servicePath := filepath.Join(realParent, "agent-sessions.service")
	if err := os.WriteFile(servicePath, []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(realParent, "agent-sessions")
	if err := os.Symlink("candidate", aliasPath); err != nil {
		t.Fatal(err)
	}
	indirectParent := filepath.Join(root, "indirect")
	if err := os.Symlink(realParent, indirectParent); err != nil {
		t.Fatal(err)
	}
	if err := restoreHostSurfaceService(hostSurfaceServiceRollback{
		Path: filepath.Join(indirectParent, "agent-sessions.service"), Present: true,
		Mode: 0o600, Body: []byte("prior"),
	}, nil); err == nil {
		t.Fatal("service restore followed an intermediate parent symlink")
	}
	if err := restoreHostSurfaceAlias(hostSurfaceAliasRollback{
		Path: filepath.Join(indirectParent, "agent-sessions"), Present: true, Target: "prior",
	}, nil); err == nil {
		t.Fatal("alias restore followed an intermediate parent symlink")
	}
	if body, err := os.ReadFile(servicePath); err != nil || string(body) != "candidate" {
		t.Fatalf("rejected indirect service restore mutated outside content: %q, %v", body, err)
	}
	if target, err := os.Readlink(aliasPath); err != nil || target != "candidate" {
		t.Fatalf("rejected indirect alias restore mutated outside link: %q, %v", target, err)
	}
}

func TestHostSurfaceRestoreDetectsParentDirectorySwap(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "bin")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(parent, "agent-sessions")
	if err := os.Symlink("candidate", aliasPath); err != nil {
		t.Fatal(err)
	}
	movedParent := filepath.Join(root, "bin-opened")
	err := restoreHostSurfaceAlias(
		hostSurfaceAliasRollback{Path: aliasPath, Present: true, Target: "prior"},
		&hostSurfaceMutationHooks{beforeMutation: func() {
			if renameErr := os.Rename(parent, movedParent); renameErr != nil {
				t.Fatal(renameErr)
			}
			if mkdirErr := os.Mkdir(parent, 0o700); mkdirErr != nil {
				t.Fatal(mkdirErr)
			}
			if symlinkErr := os.Symlink("new-parent-candidate", aliasPath); symlinkErr != nil {
				t.Fatal(symlinkErr)
			}
		}},
	)
	if err == nil {
		t.Fatal("alias restore falsely completed after its parent pathname was swapped")
	}
	if target, readErr := os.Readlink(aliasPath); readErr != nil || target != "new-parent-candidate" {
		t.Fatalf("rejected parent swap mutated replacement directory: %q, %v", target, readErr)
	}
	if target, readErr := os.Readlink(filepath.Join(movedParent, "agent-sessions")); readErr != nil || target != "candidate" {
		t.Fatalf("rejected parent swap mutated opened directory: %q, %v", target, readErr)
	}
}

func TestForwardHostServiceInstallRejectsIndirectSourceAndTarget(t *testing.T) {
	t.Run("indirect source", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "configuration"))
		source := filepath.Join(home, "source")
		if err := os.Mkdir(source, 0o700); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(home, "outside-source")
		writeTestHostServiceAsset(t, outside, []byte("program=@PREFIX@ state=@STATE_ROOT@"))
		if err := os.Symlink(filepath.Join(outside, "deploy"), filepath.Join(source, "deploy")); err != nil {
			t.Fatal(err)
		}
		if _, err := installHostServiceDefinition(source, filepath.Join(home, "prefix"), filepath.Join(home, "state")); err == nil {
			t.Fatal("forward service install followed an intermediate source symlink")
		}
		if _, err := os.Lstat(hostServiceDefinitionPath()); !os.IsNotExist(err) {
			t.Fatalf("rejected source mutated service definition: %v", err)
		}
	})

	t.Run("indirect target", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "configuration"))
		source := filepath.Join(home, "source")
		writeTestHostServiceAsset(t, source, []byte("program=@PREFIX@ state=@STATE_ROOT@"))
		outsidePath := filepath.Join(home, "outside-service")
		outside := []byte("outside")
		if err := os.WriteFile(outsidePath, outside, 0o600); err != nil {
			t.Fatal(err)
		}
		target := hostServiceDefinitionPath()
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsidePath, target); err != nil {
			t.Fatal(err)
		}
		if _, err := installHostServiceDefinition(source, filepath.Join(home, "prefix"), filepath.Join(home, "state")); err == nil {
			t.Fatal("forward service install accepted a symlink target")
		}
		if body, err := os.ReadFile(outsidePath); err != nil || string(body) != string(outside) {
			t.Fatalf("rejected target symlink mutated outside service: %q, %v", body, err)
		}
	})
}

func TestForwardHostServiceInstallDetectsParentSwapAndSyncFault(t *testing.T) {
	t.Run("target identity swap", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "configuration"))
		source := filepath.Join(home, "source")
		writeTestHostServiceAsset(t, source, []byte("program=@PREFIX@ state=@STATE_ROOT@"))
		target := hostServiceDefinitionPath()
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("prior"), 0o640); err != nil {
			t.Fatal(err)
		}
		_, err := installHostServiceDefinitionWithHooks(
			source, filepath.Join(home, "prefix"), filepath.Join(home, "state"),
			&hostSurfaceMutationHooks{beforeMutation: func() {
				if renameErr := os.Rename(target, target+".snapshotted"); renameErr != nil {
					t.Fatal(renameErr)
				}
				if writeErr := os.WriteFile(target, []byte("interloper"), 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
			}},
		)
		if err == nil {
			t.Fatal("forward service install accepted a changed post-snapshot target identity")
		}
		if body, readErr := os.ReadFile(target); readErr != nil || string(body) != "prior" {
			t.Fatalf("rejected target identity swap did not restore exact snapshot: %q, %v", body, readErr)
		}
	})

	t.Run("parent swap", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "configuration"))
		source := filepath.Join(home, "source")
		writeTestHostServiceAsset(t, source, []byte("program=@PREFIX@ state=@STATE_ROOT@"))
		target := hostServiceDefinitionPath()
		parent := filepath.Dir(target)
		movedParent := parent + ".opened"
		_, err := installHostServiceDefinitionWithHooks(
			source, filepath.Join(home, "prefix"), filepath.Join(home, "state"),
			&hostSurfaceMutationHooks{beforeMutation: func() {
				if renameErr := os.Rename(parent, movedParent); renameErr != nil {
					t.Fatal(renameErr)
				}
				if mkdirErr := os.Mkdir(parent, 0o700); mkdirErr != nil {
					t.Fatal(mkdirErr)
				}
			}},
		)
		if err == nil {
			t.Fatal("forward service install falsely completed across a parent swap")
		}
		if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
			t.Fatalf("rejected parent swap mutated replacement parent: %v", statErr)
		}
		if entries, readErr := os.ReadDir(movedParent); readErr != nil || len(entries) != 0 {
			t.Fatalf("rejected parent swap retained mutation in opened parent: %v, entries=%v", readErr, entries)
		}
	})

	t.Run("parent sync fault", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "configuration"))
		source := filepath.Join(home, "source")
		writeTestHostServiceAsset(t, source, []byte("program=@PREFIX@ state=@STATE_ROOT@"))
		target := hostServiceDefinitionPath()
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("prior"), 0o640); err != nil {
			t.Fatal(err)
		}
		syncFailure := errors.New("injected forward service parent sync failure")
		_, err := installHostServiceDefinitionWithHooks(
			source, filepath.Join(home, "prefix"), filepath.Join(home, "state"),
			&hostSurfaceMutationHooks{syncDirectory: func(*os.File) error { return syncFailure }},
		)
		if !errors.Is(err, syncFailure) {
			t.Fatalf("forward service sync fault = %v", err)
		}
		body, readErr := os.ReadFile(target)
		info, statErr := os.Stat(target)
		if readErr != nil || statErr != nil || string(body) != "prior" || info.Mode().Perm() != 0o640 {
			t.Fatalf("ambiguous forward service failure did not restore exact prior state: %q, %v, %v", body, readErr, statErr)
		}
	})
}

func TestForwardHostAliasInstallRejectsIndirectAndSwappedParents(t *testing.T) {
	t.Run("target appears after snapshot", func(t *testing.T) {
		prefix := t.TempDir()
		binRoot := filepath.Join(prefix, "bin")
		if err := os.Mkdir(binRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		firstAlias := filepath.Join(binRoot, hostAliasNames()[0])
		_, err := installHostAliasesWithHooks(prefix, &hostSurfaceMutationHooks{beforeMutation: func() {
			if symlinkErr := os.Symlink("interloper", firstAlias); symlinkErr != nil {
				t.Fatal(symlinkErr)
			}
		}})
		if err == nil {
			t.Fatal("forward alias install accepted a target that appeared after its absence snapshot")
		}
		if _, statErr := os.Lstat(firstAlias); !os.IsNotExist(statErr) {
			t.Fatalf("rejected post-snapshot alias was not rolled back: %v", statErr)
		}
	})

	t.Run("indirect parent", func(t *testing.T) {
		root := t.TempDir()
		prefix := filepath.Join(root, "prefix")
		if err := os.Mkdir(prefix, 0o700); err != nil {
			t.Fatal(err)
		}
		outsideBin := filepath.Join(root, "outside-bin")
		if err := os.Mkdir(outsideBin, 0o700); err != nil {
			t.Fatal(err)
		}
		outsideAlias := filepath.Join(outsideBin, "agent-sessions")
		if err := os.Symlink("outside", outsideAlias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideBin, filepath.Join(prefix, "bin")); err != nil {
			t.Fatal(err)
		}
		if _, err := installHostAliases(prefix); err == nil {
			t.Fatal("forward alias install followed an intermediate parent symlink")
		}
		if target, err := os.Readlink(outsideAlias); err != nil || target != "outside" {
			t.Fatalf("rejected indirect alias install mutated outside alias: %q, %v", target, err)
		}
	})

	t.Run("parent swap", func(t *testing.T) {
		root := t.TempDir()
		prefix := filepath.Join(root, "prefix")
		binRoot := filepath.Join(prefix, "bin")
		if err := os.MkdirAll(binRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		movedBin := filepath.Join(prefix, "bin.opened")
		_, err := installHostAliasesWithHooks(prefix, &hostSurfaceMutationHooks{beforeMutation: func() {
			if renameErr := os.Rename(binRoot, movedBin); renameErr != nil {
				t.Fatal(renameErr)
			}
			if mkdirErr := os.Mkdir(binRoot, 0o700); mkdirErr != nil {
				t.Fatal(mkdirErr)
			}
		}})
		if err == nil {
			t.Fatal("forward alias install falsely completed across a parent swap")
		}
		if entries, readErr := os.ReadDir(binRoot); readErr != nil || len(entries) != 0 {
			t.Fatalf("rejected alias parent swap mutated replacement directory: %v, entries=%v", readErr, entries)
		}
		if entries, readErr := os.ReadDir(movedBin); readErr != nil || len(entries) != 0 {
			t.Fatalf("rejected alias parent swap retained mutation in opened directory: %v, entries=%v", readErr, entries)
		}
	})
}

func TestForwardHostAliasInstallSyncFaultRestoresExactPriorState(t *testing.T) {
	prefix := t.TempDir()
	syncFailure := errors.New("injected forward alias parent sync failure")
	if _, err := installHostAliasesWithHooks(prefix, &hostSurfaceMutationHooks{
		syncDirectory: func(*os.File) error { return syncFailure },
	}); !errors.Is(err, syncFailure) {
		t.Fatalf("forward alias sync fault = %v", err)
	}
	for _, name := range hostAliasNames() {
		if _, err := os.Lstat(filepath.Join(prefix, "bin", name)); !os.IsNotExist(err) {
			t.Fatalf("ambiguous forward alias failure retained %q: %v", name, err)
		}
	}
}

func TestForwardHostAliasInstallRestoresEarlierAliasesWhenLaterSnapshotFails(t *testing.T) {
	prefix := t.TempDir()
	binRoot := filepath.Join(prefix, "bin")
	if err := os.Mkdir(binRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	names := hostAliasNames()
	if len(names) < 2 {
		t.Fatal("host alias inventory does not exercise a later snapshot")
	}
	blocker := filepath.Join(binRoot, names[1])
	if err := os.WriteFile(blocker, []byte("do not replace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installHostAliases(prefix); err == nil {
		t.Fatal("forward alias install accepted a later non-symlink inventory entry")
	}
	if _, err := os.Lstat(filepath.Join(binRoot, names[0])); !os.IsNotExist(err) {
		t.Fatalf("failed later alias snapshot retained earlier forward mutation: %v", err)
	}
	if body, err := os.ReadFile(blocker); err != nil || string(body) != "do not replace" {
		t.Fatalf("failed later alias snapshot mutated blocker: %q, %v", body, err)
	}
}

func writeTestHostServiceAsset(t *testing.T, source string, body []byte) {
	t.Helper()
	relative := filepath.Join("deploy", "agent-sessions", "systemd", "user", "agent-sessions.service")
	if runtime.GOOS == "darwin" {
		relative = filepath.Join("deploy", "agent-sessions", "launchd", "net.antst.agent-sessions.plist")
	}
	asset := filepath.Join(source, relative)
	if err := os.MkdirAll(filepath.Dir(asset), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asset, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
