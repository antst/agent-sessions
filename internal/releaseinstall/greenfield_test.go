package releaseinstall_test

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

var legacySurfaceNames = []string{
	"agent-session-runtime",
	"claude-code-peer",
	"codex-claude-peer",
	"codex-messaging",
	"peer-federator",
	"PEER_FEDERATOR",
	"CLAUDE_PEER_DATA_DIR",
	"CLAUDE_PEER_SUPERVISOR_SOCKET",
	"CLAUDE_PEER_APP_SERVER_SOCKET",
}

var preUnificationCompatibilityMarkers = []string{
	"cleanupStaleBridgeArtifacts",
	"corroboratedLegacyClaudeLaneManager",
	"stopLegacySupervisor",
	"legacy supervisor",
	"legacy private Claude lane",
	"tokenless legacy",
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve greenfield test location")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func TestActiveReleaseSurfacesContainNoLegacyProcessOrStateNames(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	active := make([]string, 0, 32)
	active = append(active,
		"Makefile",
		"cmd/agent-sessions",
		"cmd/agent-sessions-hub",
		".claude-plugin",
		".codex-plugin",
		".mcp.json",
		"claude",
		"deploy/agent-sessions",
		"deploy/agent-sessions-hub",
		"grok",
		"hooks",
		"qwen",
		"scripts/install-host",
		"scripts/install-hub",
		"scripts/managed-tree",
		"scripts/native-entry",
		"scripts/package-release",
		"scripts/release-inventory",
		"scripts/remove-host",
		"scripts/remove-hub",
		"skills",
	)
	active = append(active, executableDependencySources(t, root)...)

	var violations []string
	seen := make(map[string]struct{})
	for _, relative := range active {
		path := relative
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, relative)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat active release surface %s: %v", path, err)
		}
		if info.IsDir() {
			err = filepath.WalkDir(path, func(candidate string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.IsDir() || strings.HasSuffix(candidate, "_test.go") {
					return nil
				}
				collectLegacySurfaceViolations(candidate, root, seen, &violations)
				return nil
			})
			if err != nil {
				t.Fatalf("scan active release surface %s: %v", path, err)
			}
			continue
		}
		collectLegacySurfaceViolations(path, root, seen, &violations)
	}
	if len(violations) != 0 {
		sort.Strings(violations)
		t.Fatalf("active release surfaces retain pre-unification process/state or compatibility paths:\n%s", strings.Join(violations, "\n"))
	}
}

func executableDependencySources(t *testing.T, root string) []string {
	t.Helper()
	command := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.Dir}}{{end}}", "./cmd/agent-sessions", "./cmd/agent-sessions-hub")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatalf("list executable dependencies: %v", err)
	}
	var result []string
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		path := strings.TrimSpace(scanner.Text())
		if path != "" && strings.HasPrefix(path, root+string(filepath.Separator)) {
			result = append(result, path)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan executable dependency list: %v", err)
	}
	return result
}

func collectLegacySurfaceViolations(path, root string, seen map[string]struct{}, violations *[]string) {
	if _, ok := seen[path]; ok {
		return
	}
	seen[path] = struct{}{}
	body, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(body, 0) >= 0 {
		return
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		relative = path
	}
	for _, legacy := range legacySurfaceNames {
		if bytes.Contains(body, []byte(legacy)) {
			*violations = append(*violations, relative+": "+legacy)
		}
	}
	for _, marker := range preUnificationCompatibilityMarkers {
		if bytes.Contains(body, []byte(marker)) {
			*violations = append(*violations, relative+": "+marker)
		}
	}
}

func TestOperationalSurfacesCannotInvokePreUnificationCleanup(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	for _, relative := range []string{
		"Makefile", "cmd", "internal", ".claude-plugin", ".codex-plugin", ".mcp.json", "claude", "deploy", "grok", "hooks", "qwen", "skills",
		"scripts/install-host", "scripts/install-hub", "scripts/managed-tree", "scripts/native-entry", "scripts/package-release", "scripts/release-inventory", "scripts/remove-host", "scripts/remove-hub",
	} {
		path := filepath.Join(root, relative)
		err := filepath.WalkDir(path, func(candidate string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || strings.HasSuffix(candidate, "_test.go") {
				return nil
			}
			body, readErr := os.ReadFile(candidate)
			if readErr == nil && bytes.Contains(body, []byte("cleanup-pre-unification")) {
				t.Errorf("operational surface names repository-only cleanup utility: %s", candidate)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan operational surface %s: %v", path, err)
		}
	}
}

func TestMakefileHasNoPreUnificationCleanupTarget(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join(repositoryRoot(t), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, ":") {
			continue
		}
		targets, _, _ := strings.Cut(trimmed, ":")
		for _, target := range strings.Fields(targets) {
			if strings.Contains(target, "cleanup") || strings.Contains(target, "pre-unification") {
				t.Fatalf("Makefile exposes repository-only transition utility as target %q", target)
			}
		}
	}
}
