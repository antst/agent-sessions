package releaseevidence

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedServiceAcceptanceIsolatesTMPDIRBeforeInstall(t *testing.T) {
	script := filepath.Join("..", "..", "scripts", "test-unified-service")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		`"$outer_root/tmp"`,
		`TMPDIR="$outer_root/tmp"`,
		`acceptance_tmp="$acceptance_root/tmp"`,
		`export TMPDIR="$acceptance_tmp"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("unified service acceptance omitted isolated temp contract %q", required)
		}
	}
	export := strings.Index(text, `export TMPDIR="$acceptance_tmp"`)
	install := strings.Index(text, "make -s install-all")
	if export < 0 || install < 0 || export >= install {
		t.Fatalf("acceptance TMPDIR is not isolated before production install: export=%d install=%d", export, install)
	}
}

func TestReleaseTagPreflightRejectsLocalAndRemoteCollisions(t *testing.T) {
	source := filepath.Join("..", "..", "scripts", "release-tag-preflight")
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	root, remote := filepath.Join(t.TempDir(), "work"), filepath.Join(t.TempDir(), "remote.git")
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "deploy", "agent-sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "release-tag-preflight"), body, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deploy", "agent-sessions", "VERSION"), []byte("0.2.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.name", "Release Test")
	runGit(t, root, "config", "user.email", "release@example.invalid")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "fixture")
	runGit(t, "", "init", "--bare", "-q", remote)
	runGit(t, root, "remote", "add", "origin", remote)

	preflight := filepath.Join(root, "scripts", "release-tag-preflight")
	if output, err := exec.Command(preflight, "v0.2.4", "origin").CombinedOutput(); err != nil {
		t.Fatalf("absent tag was rejected: %v: %s", err, output)
	}
	runGit(t, root, "tag", "v0.2.4")
	output, err := exec.Command(preflight, "v0.2.4", "origin").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "local release tag") {
		t.Fatalf("local collision = %v: %s", err, output)
	}
	runGit(t, root, "push", "-q", "origin", "refs/tags/v0.2.4")
	runGit(t, root, "tag", "-d", "v0.2.4")
	output, err = exec.Command(preflight, "v0.2.4", "origin").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "remote release tag") {
		t.Fatalf("remote collision = %v: %s", err, output)
	}
}

func TestReleasePublicationPreflightRejectsReleaseAndAssetCollisionsSeparately(t *testing.T) {
	root := t.TempDir()
	gh := filepath.Join(root, "gh")
	if err := os.WriteFile(gh, []byte(`#!/bin/sh
if [ "$1 $2" = "release view" ]; then
	if [ "${FAKE_RELEASE_EXISTS:-}" = 1 ]; then
		printf '%s\n' "${FAKE_TARGET_ASSETS:-}"
		exit 0
	fi
	if [ "${FAKE_GH_FAILURE:-}" = 1 ]; then
		printf '%s\n' "authentication failed" >&2
		exit 1
	fi
	printf '%s\n' "release not found" >&2
	exit 1
fi
exit 2
`), 0o700); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "release-publication-preflight"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(extra ...string) ([]byte, error) {
		command := exec.Command(script, "v9.8.7", "agent-sessions-9.8.7-linux-x64.tar.gz")
		command.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "GITHUB_REPOSITORY=antst/agent-sessions")
		command.Env = append(command.Env, extra...)
		return command.CombinedOutput()
	}
	if output, err := run(); err != nil {
		t.Fatalf("unused release boundary was rejected: %v: %s", err, output)
	}
	if output, err := run("FAKE_RELEASE_EXISTS=1"); err == nil || !strings.Contains(string(output), "release v9.8.7 already exists") {
		t.Fatalf("existing release collision = %v: %s", err, output)
	}
	if output, err := run("FAKE_RELEASE_EXISTS=1", "FAKE_TARGET_ASSETS=agent-sessions-9.8.7-linux-x64.tar.gz"); err == nil || !strings.Contains(string(output), "release asset") {
		t.Fatalf("existing asset collision = %v: %s", err, output)
	}
	if output, err := run("FAKE_GH_FAILURE=1"); err == nil ||
		!strings.Contains(string(output), "failed to inspect GitHub release") ||
		!strings.Contains(string(output), "authentication failed") {
		t.Fatalf("GitHub inspection failure = %v: %s", err, output)
	}
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	if directory != "" {
		command.Dir = directory
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}
