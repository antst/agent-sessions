package releaseevidence

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestCIWorkflowGatesUnifiedReleaseOnBothPlatforms(t *testing.T) {
	root := evidenceRepositoryRoot(t)
	body, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(body)

	for _, gate := range []struct {
		job     string
		command string
	}{
		{job: "lint", command: "make lint"},
		{job: "test", command: "make test"},
		{job: "race", command: "make test-race"},
		{job: "vet", command: "go vet ./..."},
	} {
		job := ciWorkflowJob(t, workflow, gate.job)
		for _, token := range []string{
			"os: [ubuntu-latest, macos-latest]",
			"runs-on: ${{ matrix.os }}",
			gate.command,
		} {
			if !strings.Contains(job, token) {
				t.Errorf("CI %s job omits %q", gate.job, token)
			}
		}
	}

	build := ciWorkflowJob(t, workflow, "build")
	for _, token := range []string{
		"needs: inventory",
		"matrix: ${{ fromJSON(needs.inventory.outputs.matrix) }}",
		"make build-release-platform",
		"dist/release/*.tar.gz.sha256",
		"actions/upload-artifact@",
		"if-no-files-found: error",
	} {
		if !strings.Contains(build, token) {
			t.Errorf("CI four-platform two-binary build job omits %q", token)
		}
	}

	for _, jobName := range []string{"candidate-evidence", "release"} {
		job := ciWorkflowJob(t, workflow, jobName)
		if !strings.Contains(job, "specs/002-unified-user-daemon/contracts/release-evidence.schema.json") ||
			strings.Contains(job, "specs/001-qwen-support/contracts/release-evidence.schema.json") {
			t.Errorf("CI %s job does not consume the unified 0.3 evidence contract", jobName)
		}
	}

	packaging := ciWorkflowJob(t, workflow, "package-contract")
	for _, token := range []string{
		"needs: [inventory, build]",
		"release-inventory host-package-paths",
		"release-inventory hub-package-paths",
		"go test ./internal/releasepkg ./internal/releaseevidence",
	} {
		if !strings.Contains(packaging, token) {
			t.Errorf("CI host/hub package contract job omits %q", token)
		}
	}

	service := ciWorkflowJob(t, workflow, "service-fixture")
	for _, token := range []string{
		"os: [ubuntu-latest, macos-latest]",
		"runs-on: ${{ matrix.os }}",
		"AGENT_SESSIONS_CLEAN_ACCEPTANCE_USER: ${{ matrix.os == 'macos-latest' && '1' || '0' }}",
		"./scripts/test-unified-service",
	} {
		if !strings.Contains(service, token) {
			t.Errorf("CI installed service fixture job omits %q", token)
		}
	}
	for _, jobName := range []string{"test", "service-fixture"} {
		job := ciWorkflowJob(t, workflow, jobName)
		if !strings.Contains(job,
			"AGENT_SESSIONS_CLEAN_ACCEPTANCE_USER: ${{ matrix.os == 'macos-latest' && '1' || '0' }}") {
			t.Errorf("CI %s job does not derive clean-user acceptance from its matrix OS", jobName)
		}
		steps := strings.Index(job, "\n    steps:\n")
		if steps < 0 {
			t.Errorf("CI %s job has no bounded steps", jobName)
		} else if strings.Contains(job[:steps], "runner.os") {
			t.Errorf("CI %s job uses runner context in job-level env, which GitHub rejects before scheduling", jobName)
		}
		if !strings.Contains(job, "./scripts/ci-run-as-clean-systemd-user") {
			t.Errorf("CI %s job does not exercise Linux under a dedicated real systemd user", jobName)
		}
	}

	for _, releaseJob := range []string{"release-linux", "release-macos", "release"} {
		job := ciWorkflowJob(t, workflow, releaseJob)
		if !strings.Contains(job, "package-contract") || !strings.Contains(job, "service-fixture") {
			t.Errorf("CI %s job is not gated by host/hub packaging and both-platform service fixtures", releaseJob)
		}
	}
}

func ciWorkflowJob(t *testing.T, workflow, name string) string {
	t.Helper()
	header := "\n  " + name + ":\n"
	start := strings.Index(workflow, header)
	if start < 0 {
		t.Fatalf("CI workflow omits %s job", name)
	}
	start++
	remainder := workflow[start+len(header)-1:]
	nextJob := regexp.MustCompile(`(?m)^  [[:alnum:]_-]+:\n`).FindStringIndex(remainder)
	if nextJob == nil {
		return workflow[start:]
	}
	return workflow[start : start+len(header)-1+nextJob[0]]
}
