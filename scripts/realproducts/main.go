// Command realproducts executes exact authenticated acceptance cells through an explicit cell driver.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/antst/sessionbus/internal/acceptance"
	"github.com/antst/sessionbus/internal/pathidentity"
	"github.com/antst/sessionbus/internal/releaseevidence"
)

const maxCellResultBytes = 1 << 20

type options struct {
	repositoryRoot string
	requested      []string
	driver         string
	evidenceDir    string
	temporaryRoot  string
	vendor         map[string]string
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "test-real-products: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	if standingMatrixRequested(args) {
		return runStandingMatrix(ctx, args, output)
	}
	config, err := parseOptions(args)
	if err != nil {
		return err
	}
	manifest, err := releaseevidence.LoadAcceptanceManifest(filepath.Join(config.repositoryRoot, "specs", "002-unified-user-daemon", "contracts", "acceptance-matrix.yml"))
	if err != nil {
		return err
	}
	cells, err := manifest.ValidateAndExpand(config.repositoryRoot)
	if err != nil {
		return err
	}
	ordered, err := executionOrder(cells, config.requested)
	if err != nil {
		return err
	}
	if err := resolveRequiredVendors(config, ordered); err != nil {
		return err
	}

	testRoot, err := os.MkdirTemp(config.temporaryRoot, "agent-sessions-real-products.")
	if err != nil {
		return fmt.Errorf("create test-owned root: %w", err)
	}
	if err := os.Chmod(testRoot, 0o700); err != nil { //nolint:gosec // private directories require owner traversal.
		return fmt.Errorf("secure test-owned root: %w", err)
	}
	defer func() { _ = removeTestRoot(testRoot) }()

	results := make([]acceptance.Result, 0, len(ordered))
	for _, cell := range ordered {
		result, err := executeCell(ctx, config, testRoot, cell)
		if err != nil {
			return err
		}
		results = append(results, result)
	}
	acceptanceRun := acceptance.Run{RequestedCellIDs: config.requested, Results: results}
	if err := acceptance.ValidateResults(manifest, cells, acceptanceRun); err != nil {
		return fmt.Errorf("validate per-cell results: %w", err)
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(acceptanceRun)
}

func parseOptions(args []string) (options, error) {
	set := flag.NewFlagSet("test-real-products", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	repositoryRoot := set.String("repository-root", ".", "repository root")
	cells := set.String("cells", "", "comma-separated exact acceptance cell IDs")
	driver := set.String("driver", "", "absolute executable exact-cell driver")
	evidenceDir := set.String("evidence-dir", "", "existing absolute evidence directory")
	temporaryRoot := set.String("temporary-root", shortTemporaryRoot(), "existing absolute temporary parent")
	vendorFlags := map[string]*string{}
	for _, product := range []string{"codex", "claude", "grok", "qwen"} {
		vendorFlags[product] = set.String(product+"-binary", "", "installed "+product+" executable")
	}
	if err := set.Parse(args); err != nil {
		return options{}, err
	}
	if set.NArg() != 0 {
		return options{}, errors.New("unexpected positional arguments")
	}
	root, err := existingDirectory(*repositoryRoot)
	if err != nil {
		return options{}, fmt.Errorf("repository root: %w", err)
	}
	driverPath, err := existingExecutable(*driver)
	if err != nil {
		return options{}, fmt.Errorf("cell driver: %w", err)
	}
	evidence, err := existingDirectory(*evidenceDir)
	if err != nil {
		return options{}, fmt.Errorf("evidence directory: %w", err)
	}
	temporary, err := existingDirectory(*temporaryRoot)
	if err != nil {
		return options{}, fmt.Errorf("temporary root: %w", err)
	}
	requested, err := exactCellIDs(*cells)
	if err != nil {
		return options{}, err
	}
	config := options{
		repositoryRoot: root, requested: requested, driver: driverPath,
		evidenceDir: evidence, temporaryRoot: temporary, vendor: map[string]string{},
	}
	for product, value := range vendorFlags {
		config.vendor[product] = *value
	}
	return config, nil
}

func executeCell(ctx context.Context, config options, testRoot string, cell releaseevidence.AcceptanceCell) (acceptance.Result, error) {
	cellRoot := filepath.Join(testRoot, strings.ToLower(strings.ReplaceAll(cell.ID, "-", "_")))
	if err := os.Mkdir(cellRoot, 0o700); err != nil {
		return acceptance.Result{}, fmt.Errorf("create test-owned root for %s: %w", cell.ID, err)
	}
	workspace := filepath.Join(cellRoot, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		return acceptance.Result{}, fmt.Errorf("create clean workspace for %s: %w", cell.ID, err)
	}
	resultPath := filepath.Join(cellRoot, "result.json")
	// config.driver is a canonical executable and cell.ID came from the closed manifest.
	command := exec.CommandContext(ctx, config.driver, "--cell", cell.ID) //nolint:gosec
	command.Env = append(os.Environ(),
		"AGENT_SESSIONS_ACCEPTANCE_CELL_ID="+cell.ID,
		"AGENT_SESSIONS_ACCEPTANCE_TEST_ROOT="+cellRoot,
		"AGENT_SESSIONS_ACCEPTANCE_WORKSPACE="+workspace,
		"AGENT_SESSIONS_ACCEPTANCE_RESULT_PATH="+resultPath,
		"AGENT_SESSIONS_ACCEPTANCE_EVIDENCE_DIR="+config.evidenceDir,
	)
	for product, path := range config.vendor {
		if path != "" {
			command.Env = append(command.Env, "AGENT_SESSIONS_ACCEPTANCE_"+strings.ToUpper(product)+"_BINARY="+path)
		}
	}
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	runErr := command.Run()
	var cleanupErr error
	if command.Process != nil {
		cleanupErr = terminateOwnedProcessGroup(command.Process.Pid)
	}
	if runErr != nil {
		return acceptance.Result{}, fmt.Errorf("cell %s driver failed: %w", cell.ID, errors.Join(runErr, cleanupErr))
	}
	if cleanupErr != nil {
		return acceptance.Result{}, fmt.Errorf("cell %s left a test-owned process group: %w", cell.ID, cleanupErr)
	}
	result, err := readCellResult(resultPath)
	if err != nil {
		return acceptance.Result{}, fmt.Errorf("cell %s: %w", cell.ID, err)
	}
	if result.CellID != cell.ID {
		return acceptance.Result{}, fmt.Errorf("cell driver returned %q for requested %q", result.CellID, cell.ID)
	}
	if err := validateEvidencePaths(config.evidenceDir, result.EvidencePaths); err != nil {
		return acceptance.Result{}, fmt.Errorf("cell %s: %w", cell.ID, err)
	}
	return result, nil
}

func validateEvidencePaths(evidenceRoot string, paths []string) error {
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if filepath.IsAbs(path) {
			return fmt.Errorf("evidence path %q must be relative", path)
		}
		clean := filepath.Clean(path)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("evidence path %q must stay inside evidence directory", path)
		}
		if _, duplicate := seen[clean]; duplicate {
			return fmt.Errorf("duplicate evidence path %q", clean)
		}
		seen[clean] = struct{}{}
		identity, err := pathidentity.ExistingNoFollow(filepath.Join(evidenceRoot, clean))
		if err != nil || identity.Kind != pathidentity.KindRegular {
			return fmt.Errorf("evidence path %q is not an existing regular file", path)
		}
		info, err := os.Lstat(identity.Path)
		if err != nil || info.Size() <= 0 {
			return fmt.Errorf("evidence path %q is not a nonempty regular file", path)
		}
	}
	return nil
}

func readCellResult(path string) (acceptance.Result, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return acceptance.Result{}, fmt.Errorf("stat result: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxCellResultBytes {
		return acceptance.Result{}, errors.New("result is not one bounded no-follow regular file")
	}
	// path was lstat-verified as one bounded, no-follow regular file above.
	body, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return acceptance.Result{}, fmt.Errorf("read result: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var result acceptance.Result
	if err := decoder.Decode(&result); err != nil {
		return acceptance.Result{}, fmt.Errorf("decode result: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return acceptance.Result{}, errors.New("result contains trailing JSON")
	}
	return result, nil
}

func executionOrder(cells []releaseevidence.AcceptanceCell, requested []string) ([]releaseevidence.AcceptanceCell, error) {
	byID := make(map[string]releaseevidence.AcceptanceCell, len(cells))
	for _, cell := range cells {
		byID[cell.ID] = cell
	}
	seen := map[string]bool{}
	visiting := map[string]bool{}
	ordered := make([]releaseevidence.AcceptanceCell, 0)
	var visit func(string) error
	visit = func(id string) error {
		cell, ok := byID[id]
		if !ok {
			return fmt.Errorf("unknown requested acceptance cell %q", id)
		}
		if seen[id] {
			return nil
		}
		if visiting[id] {
			return fmt.Errorf("acceptance prerequisite cycle at %q", id)
		}
		visiting[id] = true
		for _, prerequisite := range cell.Prerequisites {
			if err := visit(prerequisite); err != nil {
				return err
			}
		}
		visiting[id] = false
		seen[id] = true
		ordered = append(ordered, cell)
		return nil
	}
	for _, id := range requested {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func resolveRequiredVendors(config options, cells []releaseevidence.AcceptanceCell) error {
	required := map[string]bool{}
	for _, cell := range cells {
		for _, product := range productsForCell(cell) {
			required[product] = true
		}
	}
	for product := range required {
		candidate := config.vendor[product]
		if candidate == "" {
			candidate, _ = exec.LookPath(product)
		}
		resolved, err := existingExecutable(candidate)
		if err != nil {
			return fmt.Errorf("real installed %s binary: %w", product, err)
		}
		config.vendor[product] = resolved
	}
	return nil
}

func productsForCell(cell releaseevidence.AcceptanceCell) []string {
	return releaseevidence.ProductsForAcceptanceCell(cell)
}

func exactCellIDs(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("--cells requires one or more exact cell IDs")
	}
	seen := map[string]bool{}
	var result []string
	for _, value := range strings.Split(raw, ",") {
		id := strings.TrimSpace(value)
		if id == "" {
			return nil, errors.New("--cells contains an empty cell ID")
		}
		if seen[id] {
			return nil, fmt.Errorf("--cells contains duplicate %q", id)
		}
		seen[id] = true
		result = append(result, id)
	}
	return result, nil
}

func existingDirectory(raw string) (string, error) {
	if !filepath.IsAbs(raw) {
		absolute, err := filepath.Abs(raw)
		if err != nil {
			return "", err
		}
		raw = absolute
	}
	identity, err := pathidentity.ExistingNoFollow(raw)
	if err != nil || identity.Kind != pathidentity.KindDirectory {
		return "", errors.New("path is not an existing real directory")
	}
	return identity.Path, nil
}

func existingExecutable(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("path is empty")
	}
	if !filepath.IsAbs(raw) {
		found, err := exec.LookPath(raw)
		if err != nil {
			return "", err
		}
		raw = found
	}
	canonical, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("path is not an executable regular file")
	}
	return canonical, nil
}

func removeTestRoot(root string) error {
	identity, err := pathidentity.ExistingNoFollow(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || identity.Kind != pathidentity.KindDirectory || identity.Path != filepath.Clean(root) {
		return errors.New("refuse to remove unverified test-owned root")
	}
	return os.RemoveAll(root)
}

//nolint:gocyclo // TERM/KILL identity checks intentionally fail closed at every transition.
func terminateOwnedProcessGroup(processGroupID int) error {
	if processGroupID <= 1 {
		return errors.New("test-owned process group identity is invalid")
	}
	alive := func() (bool, error) {
		err := syscall.Kill(-processGroupID, 0)
		if err == nil || errors.Is(err, syscall.EPERM) {
			// EPERM still proves that the group exists. Let the actual TERM or
			// KILL operation decide whether cleanup authority is available;
			// Darwin may report EPERM for the zero-signal probe while a
			// subsequent signal to the test-owned group succeeds.
			return true, nil
		}
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, err
	}
	remaining, err := alive()
	if err != nil {
		return err
	}
	if !remaining {
		return nil
	}
	if err := syscall.Kill(-processGroupID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		remaining, err = alive()
		if err != nil || !remaining {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(-processGroupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		remaining, err = alive()
		if err != nil || !remaining {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("test-owned process group did not exit")
}

func shortTemporaryRoot() string {
	if runtime.GOOS == "darwin" {
		return "/tmp"
	}
	return os.TempDir()
}
