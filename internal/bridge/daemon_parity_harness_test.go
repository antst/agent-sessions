package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/antst/agent-sessions/internal/releaseevidence"
)

const legacyParityPackageDeadline = 5 * time.Minute

type legacyGoTestEvent struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
	Output string `json:"Output"`
}

// runMappedLegacyParity executes the exact baseline tests named by the public
// port map. The child test processes preserve the original package boundary,
// TestMain behavior, helper-process argv, and native environment semantics.
// A missing, skipped, or renamed test is a parity failure rather than credit.
func runMappedLegacyParity(t *testing.T, entryIDs ...string) {
	t.Helper()
	repositoryRoot := legacyParityRepositoryRoot(t)
	portMap, err := releaseevidence.LoadBaselinePortMap(filepath.Join(
		repositoryRoot,
		"specs/002-unified-user-daemon/contracts/baseline-port-map.yml",
	))
	if err != nil {
		t.Fatalf("load baseline port map: %v", err)
	}

	entries := make(map[string]releaseevidence.BaselinePortEntry, len(portMap.Entries))
	for _, entry := range portMap.Entries {
		entries[entry.ID] = entry
	}
	references := make([]string, 0)
	for _, entryID := range entryIDs {
		entry, ok := entries[entryID]
		if !ok {
			t.Fatalf("baseline parity entry %s is not defined", entryID)
		}
		if len(entry.OldTests) == 0 {
			t.Fatalf("baseline parity entry %s has no mapped tests", entryID)
		}
		references = append(references, entry.OldTests...)
	}
	runLegacyParityReferences(t, repositoryRoot, references)
}

func runLegacyParityReferences(t *testing.T, repositoryRoot string, references []string) {
	t.Helper()
	byPackage := make(map[string][]string)
	seenTests := make(map[string]string)
	for _, reference := range references {
		path, name, ok := strings.Cut(reference, ":")
		if !ok || path == "" || name == "" || strings.Contains(name, ":") || !strings.HasSuffix(path, "_test.go") {
			t.Fatalf("invalid legacy parity test reference %q", reference)
		}
		packagePath := "./" + filepath.ToSlash(filepath.Dir(path))
		if prior, duplicate := seenTests[name]; duplicate {
			t.Fatalf("legacy parity test %s is mapped by both %s and %s", name, prior, packagePath)
		}
		seenTests[name] = packagePath
		byPackage[packagePath] = append(byPackage[packagePath], name)
	}
	if len(byPackage) == 0 {
		t.Fatal("baseline parity selection is empty")
	}

	packages := make([]string, 0, len(byPackage))
	for packagePath := range byPackage {
		packages = append(packages, packagePath)
	}
	sort.Strings(packages)
	for _, packagePath := range packages {
		names := append([]string(nil), byPackage[packagePath]...)
		sort.Strings(names)
		t.Run(strings.TrimPrefix(packagePath, "./internal/"), func(t *testing.T) {
			runLegacyParityPackage(t, repositoryRoot, packagePath, names)
		})
	}
}

func runLegacyParityPackage(t *testing.T, repositoryRoot, packagePath string, names []string) {
	t.Helper()
	if len(names) == 0 {
		t.Fatalf("legacy parity package %s has no tests", packagePath)
	}
	escaped := make([]string, 0, len(names))
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, "Test") {
			t.Fatalf("legacy parity symbol %q is not a Go test", name)
		}
		escaped = append(escaped, regexp.QuoteMeta(name))
		wanted[name] = true
	}
	pattern := "^(" + strings.Join(escaped, "|") + ")$"

	ctx, cancel := context.WithTimeout(context.Background(), legacyParityPackageDeadline)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-json", "-count=1", "-run", pattern, packagePath)
	command.Dir = repositoryRoot
	command.Env = legacyParityEnvironment(os.Environ())
	if wanted["TestGrokRealCommandListHasNoPassthroughDrift"] {
		command.Env = append(command.Env, "GROK_PEER_GROK_BIN="+writeLegacyParityGrokFixture(t))
	}
	output, commandErr := command.Output()
	var exitErr *exec.ExitError
	if errors.As(commandErr, &exitErr) {
		output = append(output, exitErr.Stderr...)
	}

	status := make(map[string]string, len(names))
	var transcript strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 4<<20)
	for scanner.Scan() {
		var event legacyGoTestEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			fmt.Fprintln(&transcript, scanner.Text())
			continue
		}
		if event.Output != "" {
			transcript.WriteString(event.Output)
		}
		if wanted[event.Test] && (event.Action == "pass" || event.Action == "fail" || event.Action == "skip") {
			status[event.Test] = event.Action
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("decode %s legacy parity output: %v", packagePath, err)
	}
	if ctx.Err() != nil {
		t.Fatalf("legacy parity package %s exceeded %s\n%s", packagePath, legacyParityPackageDeadline, transcript.String())
	}
	if commandErr != nil {
		t.Fatalf("legacy parity package %s failed: %v\n%s", packagePath, commandErr, transcript.String())
	}
	for _, name := range names {
		if status[name] != "pass" {
			t.Errorf("legacy parity test %s in %s has terminal status %q, want pass", name, packagePath, status[name])
		}
	}
}

func legacyParityEnvironment(environment []string) []string {
	transient := map[string]bool{
		"AGENT_SESSIONS_AGENT_RUNTIME_DIR": true,
		"AGENT_SESSIONS_PRODUCT":           true,
		"AGENT_SESSIONS_SESSION_ID":        true,
		"CLAUDE_PEER_CLAUDE_CONFIG_DIR":    true,
		"GROK_PEER_GROK_BIN":               true,
	}
	clean := make([]string, 0, len(environment))
	for _, item := range environment {
		name, _, _ := strings.Cut(item, "=")
		if !transient[name] {
			clean = append(clean, item)
		}
	}
	return clean
}

func writeLegacyParityGrokFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grok")
	body := `#!/bin/sh
if [ "$1" = "--no-auto-update" ] && [ "$2" = "--help" ]; then
  cat <<'EOF'
Grok Build TUI
Usage: grok [COMMAND]
--leader-socket PATH
Commands:
  agent
  sessions
  worktree
  leader
  doctor
  du
  plugin
  update
  version
  models
  help
EOF
  exit 0
fi
exit 64
`
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write parity Grok fixture: %v", err)
	}
	return path
}

func legacyParityRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve legacy parity harness source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if info, err := os.Stat(filepath.Join(root, "go.mod")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("legacy parity repository root %s is invalid", root)
	}
	return root
}
