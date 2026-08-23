package releaseevidence

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
	if err := os.MkdirAll(filepath.Join(root, "deploy", "peer-federator"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "release-tag-preflight"), body, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deploy", "peer-federator", "VERSION"), []byte("0.2.4\n"), 0o600); err != nil {
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
  [ "${FAKE_RELEASE_EXISTS:-}" = 1 ] && exit 0
  exit 1
fi
if [ "$1" = api ]; then
  printf '%s\n' "${FAKE_ASSET_COLLISION:-}"
  exit 0
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
		command := exec.Command(script, "v0.2.4", "agent-sessions-0.2.4-linux-x64.tar.gz")
		command.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "GITHUB_REPOSITORY=antst/agent-sessions")
		command.Env = append(command.Env, extra...)
		return command.CombinedOutput()
	}
	if output, err := run(); err != nil {
		t.Fatalf("unused release boundary was rejected: %v: %s", err, output)
	}
	if output, err := run("FAKE_RELEASE_EXISTS=1"); err == nil || !strings.Contains(string(output), "release v0.2.4 already exists") {
		t.Fatalf("existing release collision = %v: %s", err, output)
	}
	if output, err := run("FAKE_ASSET_COLLISION=agent-sessions-0.2.4-linux-x64.tar.gz"); err == nil || !strings.Contains(string(output), "release asset") {
		t.Fatalf("existing asset collision = %v: %s", err, output)
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
