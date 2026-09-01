package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/antst/agent-sessions/internal/acceptance"
	"github.com/antst/agent-sessions/internal/releaseevidence"
)

func TestExecutionOrderIncludesEachExactPrerequisiteOnce(t *testing.T) {
	root := repositoryRoot(t)
	manifest, err := releaseevidence.LoadAcceptanceManifest(filepath.Join(root, "specs/002-unified-user-daemon/contracts/acceptance-matrix.yml"))
	if err != nil {
		t.Fatal(err)
	}
	cells, err := manifest.ValidateAndExpand(root)
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := executionOrder(cells, []string{"C-01", "C-02"})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(ordered))
	for _, cell := range ordered {
		got = append(got, cell.ID)
	}
	want := []string{"S-01", "U-01", "U-08", "C-01", "C-02"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("execution order = %v, want %v", got, want)
	}
	if _, err := executionOrder(cells, []string{"UNKNOWN"}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown cell error = %v", err)
	}
}

func TestRunnerEmitsValidatedPerCellRunAndRemovesOnlyOwnedRoot(t *testing.T) {
	root := repositoryRoot(t)
	temporary := canonicalTempDir(t)
	evidence := canonicalTempDir(t)
	driver := filepath.Join(canonicalTempDir(t), "driver")
	driverBody := `#!/bin/sh
set -eu
test "$1" = --cell
cell="$2"
test -d "$AGENT_SESSIONS_ACCEPTANCE_WORKSPACE"
test ! -e "$AGENT_SESSIONS_ACCEPTANCE_WORKSPACE/.mcp.json"
test ! -e "$AGENT_SESSIONS_ACCEPTANCE_WORKSPACE/.claude"
test ! -e "$AGENT_SESSIONS_ACCEPTANCE_WORKSPACE/.agents"
case "$(uname -s)" in Darwin) os_name=macos ;; *) os_name=linux ;; esac
sleep 60 &
printf '%s\n' "$!" >"$AGENT_SESSIONS_ACCEPTANCE_EVIDENCE_DIR/owned-child.pid"
printf '%s\n' 'destination-visible test receipt' >"$AGENT_SESSIONS_ACCEPTANCE_EVIDENCE_DIR/result.txt"
printf '%s\n' "{\"result_id\":\"result-$cell\",\"cell_id\":\"$cell\",\"verdict\":\"PASS\",\"source_commit\":\"candidate-commit\",\"source_tree\":\"candidate-tree\",\"installed_release_identity\":\"sha256:candidate\",\"os\":\"$os_name\",\"architecture\":\"test-arch\",\"native_product_versions\":{\"codex\":\"real-version\"},\"evidence_paths\":[\"result.txt\"],\"literal_command\":\"exact command\",\"cwd\":\"/exact/cwd\",\"exit_status\":0,\"exact_identity_evidence\":{\"kind\":\"real-installed\",\"artifact_identity\":\"sha256:native\"},\"preserved_state_evidence\":\"preserved\",\"cleanup_evidence\":\"clean\"}" >"$AGENT_SESSIONS_ACCEPTANCE_RESULT_PATH"
`
	if err := os.WriteFile(driver, []byte(driverBody), 0o700); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err := run(context.Background(), []string{
		"--repository-root", root,
		"--cells", "S-01",
		"--driver", driver,
		"--evidence-dir", evidence,
		"--temporary-root", temporary,
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	var result acceptance.Run
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.RequestedCellIDs) != 1 || result.RequestedCellIDs[0] != "S-01" ||
		len(result.Results) != 1 || result.Results[0].CellID != "S-01" {
		t.Fatalf("runner output = %+v", result)
	}
	matches, err := filepath.Glob(filepath.Join(temporary, "agent-sessions-real-products.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("test-owned roots survived: %v", matches)
	}
	if _, err := os.Lstat(evidence); err != nil {
		t.Fatalf("runner removed unrelated evidence directory: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(evidence, "owned-child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("test-owned child %d survived runner cleanup: %v", childPID, err)
	}
}

func TestEvidencePathsAreUniqueExistingFilesInsideEvidenceRoot(t *testing.T) {
	root := canonicalTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "receipt.txt"), []byte("receipt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateEvidencePaths(root, []string{"receipt.txt"}); err != nil {
		t.Fatalf("valid evidence rejected: %v", err)
	}
	for _, test := range []struct {
		name  string
		paths []string
		want  string
	}{
		{name: "missing", paths: []string{"missing.txt"}, want: "existing regular file"},
		{name: "absolute", paths: []string{filepath.Join(root, "receipt.txt")}, want: "relative"},
		{name: "traversal", paths: []string{"../receipt.txt"}, want: "inside evidence directory"},
		{name: "duplicate", paths: []string{"receipt.txt", "receipt.txt"}, want: "duplicate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateEvidencePaths(root, test.paths); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateEvidencePaths() error = %v, want containing %q", err, test.want)
			}
		})
	}
	if err := os.Symlink(filepath.Join(root, "receipt.txt"), filepath.Join(root, "receipt-link")); err != nil {
		t.Fatal(err)
	}
	if err := validateEvidencePaths(root, []string{"receipt-link"}); err == nil || !strings.Contains(err.Error(), "existing regular file") {
		t.Fatalf("symlink evidence error = %v", err)
	}
}

func TestRunnerRefusesDuplicateUnknownAndMissingRealVendor(t *testing.T) {
	if _, err := exactCellIDs("S-01,S-01"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate cell error = %v", err)
	}
	if _, err := exactCellIDs("S-01,"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty cell error = %v", err)
	}
	config := options{vendor: map[string]string{"codex": filepath.Join(t.TempDir(), "missing")}}
	if err := resolveRequiredVendors(config, []releaseevidence.AcceptanceCell{{Family: "codex-interactive"}}); err == nil || !strings.Contains(err.Error(), "real installed codex") {
		t.Fatalf("missing real vendor error = %v", err)
	}
}

func TestMessagingCellsResolveEveryRealVendor(t *testing.T) {
	products := productsForCell(releaseevidence.AcceptanceCell{ID: "M-CP-GP", Family: "peer-lane-messaging"})
	if strings.Join(products, ",") != "codex,grok" {
		t.Fatalf("messaging products = %v, want exact source and destination vendors", products)
	}

	config := options{vendor: map[string]string{}}
	for _, product := range products {
		path := filepath.Join(canonicalTempDir(t), product)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		config.vendor[product] = path
	}
	if err := resolveRequiredVendors(config, []releaseevidence.AcceptanceCell{{ID: "M-CP-GP", Family: "peer-lane-messaging"}}); err != nil {
		t.Fatal(err)
	}
	if len(config.vendor) != 2 {
		t.Fatalf("resolved vendors = %v, want exact two-product cell", config.vendor)
	}
	for product, path := range config.vendor {
		if !filepath.IsAbs(path) || filepath.Base(path) != product {
			t.Fatalf("resolved %s vendor = %q", product, path)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
