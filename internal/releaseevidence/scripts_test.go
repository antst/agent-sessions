package releaseevidence

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseGateManifestFindsManagedGrokOutsidePATH(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	evidence := filepath.Join(root, "gates")
	tools := filepath.Join(root, "bin", "tools")
	home := filepath.Join(root, "home")
	for _, directory := range []string{bin, evidence, tools, filepath.Join(home, ".grok", "bin")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, executable := range []string{"go", "qwen", "codex", "claude"} {
		path := filepath.Join(bin, executable)
		writeTestFile(t, path, "#!/bin/sh\nprintf '%s fixture version\\n' '"+executable+"'\n")
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, output := range map[string]string{
		filepath.Join(tools, "golangci-lint"):       "golangci-lint fixture version",
		filepath.Join(home, ".grok", "bin", "grok"): "grok fixture version",
	} {
		writeTestFile(t, path, "#!/bin/sh\nprintf '%s\\n' '"+output+"'\n")
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, gate := range []string{
		"normal", "race", "vet", "lint", "focused_contracts", "quickstart",
		"permissions", "federation", "prebuilt_install", "owner_nonmutation",
	} {
		writeTestFile(t, filepath.Join(evidence, "linux-"+gate+".txt"), "passed\n")
	}
	manifest := filepath.Join(root, "linux.json")
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "release-gate-manifest"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(script, "--os", "linux",
		"--job-url", "https://github.com/antst/agent-sessions/actions/runs/1/job/2",
		"--evidence-dir", evidence, "--output", manifest)
	command.Dir = root
	command.Env = append(os.Environ(),
		"HOME="+home, "PATH="+bin+":/usr/bin:/bin",
		"QWEN_PEER_QWEN_BIN=", "CODEX_PEER_CODEX_BIN=",
		"CLAUDE_PEER_CLAUDE_BIN=", "GROK_PEER_GROK_BIN=",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("release gate manifest: %v: %s", err, output)
	}
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		NativeClients map[string]string `json:"native_clients"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	for product, want := range map[string]string{
		"qwen": "qwen fixture version", "codex": "codex fixture version",
		"claude": "claude fixture version", "grok": "grok fixture version",
	} {
		if got := document.NativeClients[product]; got != want {
			t.Fatalf("%s fixture version = %q, want %q", product, got, want)
		}
	}
}

func TestReleaseWorkflowDownloadsExactCandidateArtifacts(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(body)
	for _, required := range []string{
		"- name: Download exact candidate platform archives",
		`gh run download "${{ steps.tag.outputs.run_id }}" --repo "$GITHUB_REPOSITORY"`,
		`--name "agent-sessions-$platform" --dir dist/release`,
		`evidence="dist/candidate/evidence/agent-sessions-v${{ needs.inventory.outputs.version }}-release-evidence.json"`,
		`--document "$evidence" --archive-dir dist/release --package-dir dist/candidate/packages`,
		`--archive-dir dist/release --package-dir dist/packages --gate-dir dist/gates`,
		"dist/packages/*.tgz",
		"id-token: write",
		`npm publish "$archive" --provenance --access public`,
		`npm view "$package_name@$version" dist.integrity`,
		`cp dist/candidate/packages/*.tgz dist/release/`,
		"publish-preview:",
		"needs: [lint, test, build]",
		`npx --yes pkg-pr-new publish "${package_paths[@]}"`,
		"actionlint .github/workflows/ci.yml",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("release workflow is missing exact-candidate boundary %q", required)
		}
	}
	if strings.Contains(workflow, "- name: Download platform archives\n        uses: actions/download-artifact") {
		t.Fatal("release workflow downloads platform archives from the tag run instead of the evidence-bound candidate run")
	}
	if strings.Contains(workflow, `npm view "$package_name@$version" version`) {
		t.Fatal("release workflow still skips packages by name and version without integrity")
	}
	preflight, publish := strings.Index(workflow, `npm view "$package_name@$version" dist.integrity`), strings.Index(workflow, `npm publish "$archive"`)
	if preflight < 0 || publish < 0 || preflight > publish || !strings.Contains(workflow[preflight:publish], `queued_archives+=("$archive")`) {
		t.Fatal("release workflow does not finish the integrity preflight before publishing")
	}
	if strings.Contains(workflow, "environment:") || strings.Contains(workflow, "NODE_AUTH_TOKEN") ||
		strings.Contains(workflow, "@agent-sessions/dsh-") || strings.Contains(workflow, "node-version: 24") {
		t.Fatal("release workflow has an environment, token, hard-coded package, or second Node version source")
	}
	if setupCount := strings.Count(workflow, "uses: actions/setup-node@"); setupCount == 0 ||
		setupCount != strings.Count(workflow, "node-version-file: deploy/agent-sessions/NODE_VERSION") {
		t.Fatal("not every Node setup consumes the authoritative Node version file")
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
	installPackageVersionFixture(t, root, "0.2.4")
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
	writeJSONTestFile(t, filepath.Join(root, "integrations", "dsh", "lane", "package.json"), map[string]any{
		"name": "@agent-sessions/dsh-lane", "version": "0.2.3",
	})
	if output, err := exec.Command(preflight, "v0.2.4", "origin").CombinedOutput(); err == nil || !strings.Contains(string(output), "does not match release") {
		t.Fatalf("package version drift = %v: %s", err, output)
	}
	writeJSONTestFile(t, filepath.Join(root, "integrations", "dsh", "lane", "package.json"), map[string]any{
		"name": "@agent-sessions/dsh-lane", "version": "0.2.4",
	})
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

func TestReleaseTagVerifyParsesTrailersBeforeSignatureBlock(t *testing.T) {
	source := filepath.Join("..", "..", "scripts", "release-tag-verify")
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, directory := range []string{
		filepath.Join(root, "scripts"),
		filepath.Join(root, "deploy", "agent-sessions"),
		filepath.Join(root, "bin"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "release-tag-verify"), body, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deploy", "agent-sessions", "VERSION"), []byte("0.2.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	installPackageVersionFixture(t, root, "0.2.4")
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.name", "Release Test")
	runGit(t, root, "config", "user.email", "release@example.invalid")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "fixture")
	commit := gitOutput(t, root, "rev-parse", "HEAD")
	tree := gitOutput(t, root, "rev-parse", "HEAD^{tree}")
	message := strings.Join([]string{
		"Agent Sessions v0.2.4",
		"",
		"Agent-Sessions-Evidence-Run: https://github.com/antst/agent-sessions/actions/runs/123",
		"Agent-Sessions-Evidence-Artifact: agent-sessions-v0.2.4-release-evidence-" + commit,
		"Agent-Sessions-Evidence-SHA256: " + strings.Repeat("a", 64),
		"-----BEGIN SSH SIGNATURE-----",
		"U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAg5vdTVet3xAIfUGUQu7w1hA8FLe",
		"xp42hPuuyRfuaCyBMAAAADZ2l0AAAAAAAAAAZzaGE1MTIAAABTAAAAC3NzaC1lZDI1NTE5",
		"AAAAQOblrYOslmldowJQaGpLTzRDxsDkhBtKgOAw0AdE9g8qMCD+4M1/qqQsefccIYhIFd",
		"wLyUOj2z/+L9XfB4cuigU=",
		"-----END SSH SIGNATURE-----",
	}, "\n")
	runGit(t, root, "tag", "-a", "v0.2.4", "-m", message)
	tagBody := gitOutput(t, root, "for-each-ref", "--format=%(contents:body)", "refs/tags/v0.2.4")
	if strings.Contains(tagBody, "BEGIN SSH SIGNATURE") || !strings.Contains(tagBody, "Agent-Sessions-Evidence-Run:") {
		t.Fatalf("tag body did not isolate signed annotation trailers: %q", tagBody)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitWrapper := filepath.Join(root, "bin", "git")
	wrapper := "#!/bin/sh\n" +
		"if [ \"$#\" -ge 3 ] && [ \"$1\" = -C ] && [ \"$3\" = verify-tag ]; then exit 0; fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(gitWrapper, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(root, "scripts", "release-tag-verify"), "v0.2.4", commit, tree)
	command.Dir = root
	command.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("signed tag trailers were rejected: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "run_id=123") || !strings.Contains(string(output), strings.Repeat("a", 64)) {
		t.Fatalf("verified tag output omitted evidence binding: %s", output)
	}
}

func installPackageVersionFixture(t *testing.T, root, version string) {
	t.Helper()
	for _, script := range []string{"release-inventory", "verify-package-versions"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "scripts", script))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "scripts", script), body, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	inventory := exec.Command(filepath.Join(root, "scripts", "release-inventory"), "packages")
	output, err := inventory.CombinedOutput()
	if err != nil {
		t.Fatalf("read package inventory fixture: %v: %s", err, output)
	}
	for _, row := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Split(row, "|")
		if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
			t.Fatalf("malformed package inventory fixture row %q", row)
		}
		directory := filepath.Join(root, filepath.FromSlash(fields[0]))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeJSONTestFile(t, filepath.Join(directory, "package.json"), map[string]any{"name": fields[1], "version": version})
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
	if output, err := run("FAKE_RELEASE_EXISTS=1", "FAKE_TARGET_ASSETS=agent-sessions-0.2.4-linux-x64.tar.gz"); err == nil || !strings.Contains(string(output), "release asset") {
		t.Fatalf("existing asset collision = %v: %s", err, output)
	}
	if output, err := run("FAKE_GH_FAILURE=1"); err == nil ||
		!strings.Contains(string(output), "failed to inspect GitHub release") ||
		!strings.Contains(string(output), "authentication failed") {
		t.Fatalf("GitHub inspection failure = %v: %s", err, output)
	}
}

func TestReleaseCommitVerifyAcceptsMaintainerOrVerifiedWebFlowSignature(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(bin, "git")
	if err := os.WriteFile(git, []byte(`#!/bin/sh
case " $* " in
  *" verify-commit "*) test "${FAKE_MAINTAINER_SIGNATURE:-}" = valid ;;
  *) exit 2 ;;
esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(bin, "gh")
	if err := os.WriteFile(gh, []byte(`#!/bin/sh
test "${FAKE_GITHUB_SIGNATURE:-}" = valid || exit 1
printf '%s\ttrue\tvalid\tweb-flow\n' "${FAKE_COMMIT:?}"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "release-commit-verify"))
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	run := func(extra ...string) ([]byte, error) {
		command := exec.Command(script, commit)
		command.Env = append(os.Environ(),
			"PATH="+bin+":"+os.Getenv("PATH"),
			"GITHUB_REPOSITORY=antst/agent-sessions",
			"RELEASE_ALLOWED_SIGNERS=release@example.invalid ssh-ed25519 AAAA",
			"FAKE_COMMIT="+commit,
		)
		command.Env = append(command.Env, extra...)
		return command.CombinedOutput()
	}
	if output, err := run("FAKE_MAINTAINER_SIGNATURE=valid"); err != nil ||
		!strings.Contains(string(output), "allowed maintainer signature") {
		t.Fatalf("maintainer signature = %v: %s", err, output)
	}
	if output, err := run("FAKE_GITHUB_SIGNATURE=valid"); err != nil ||
		!strings.Contains(string(output), "verified GitHub web-flow signature") {
		t.Fatalf("GitHub signature = %v: %s", err, output)
	}
	if output, err := run(); err == nil || !strings.Contains(string(output), "cannot verify GitHub") {
		t.Fatalf("unverified commit = %v: %s", err, output)
	}
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if directory != "" {
		command.Dir = directory
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func gitOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
