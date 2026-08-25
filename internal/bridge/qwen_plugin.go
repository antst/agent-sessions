package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/antst/agent-sessions/internal/envutil"
	"github.com/antst/agent-sessions/internal/pathidentity"
	"github.com/antst/agent-sessions/internal/procinfo"
	"github.com/antst/agent-sessions/internal/qwenprofile"
)

const (
	qwenPluginSchema = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	qwenMCPSchema    = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
	qwenPluginName   = "agent-sessions"
	qwenMCPName      = "agent_sessions"
	qwenMCPCommand   = "./scripts/native-entry"
)

var qwenPluginSkills = []string{
	"agent-sessions", "claude-lane", "codex-lane", "grok-lane", "qwen-lane",
}

func qwenPluginInstallCommand(executable, pluginRoot string, profile qwenprofile.Identity, environment []string) (*exec.Cmd, error) {
	if strings.TrimSpace(executable) == "" {
		return nil, errors.New("qwen executable is empty")
	}
	if !filepath.IsAbs(pluginRoot) {
		return nil, fmt.Errorf("qwen plugin root must be absolute: %q", pluginRoot)
	}
	command := exec.Command(executable, "extensions", "install", pluginRoot, "--scope", "user", "--consent") //nolint:gosec // Executable is the operator-selected Qwen binary; argv is structured and fixed.
	command.Env = qwenprofile.ApplyEnvironment(environment, profile)
	return command, nil
}

func qwenPluginUninstallCommand(executable string, profile qwenprofile.Identity, environment []string) *exec.Cmd {
	command := exec.Command(executable, "extensions", "uninstall", qwenPluginName)
	command.Env = qwenprofile.ApplyEnvironment(environment, profile)
	return command
}

func qwenPluginRollbackSource(root, source, version string) (string, error) {
	canonical, err := pathidentity.ExistingDirectory(source)
	if err != nil {
		return "", fmt.Errorf("prior Qwen plugin source is not an existing real directory: %w", err)
	}
	installedRoot, err := pathidentity.ExistingDirectory(root)
	if err != nil {
		return "", fmt.Errorf("installed Qwen plugin root is not an existing real directory: %w", err)
	}
	if canonical == installedRoot {
		return "", errors.New("prior Qwen plugin source is the installed plugin root and would be removed by replacement")
	}
	if version == "" {
		return "", errors.New("prior Qwen plugin version is empty")
	}
	if err := verifyQwenPluginInstallation(canonical, version, true); err != nil {
		return "", fmt.Errorf("prior Qwen plugin source does not provide the installed plugin payload: %w", err)
	}
	return canonical, nil
}

func verifyQwenPluginRollback(root, source, version, statePath string) error {
	enabled, err := qwenPluginEnabled(statePath)
	if err != nil {
		return fmt.Errorf("verify restored Qwen plugin policy: %w", err)
	}
	if !enabled {
		return errors.New("restored Qwen plugin is not enabled")
	}
	if err := verifyQwenPluginInstallation(root, version, enabled); err != nil {
		return fmt.Errorf("verify restored Qwen plugin payload: %w", err)
	}
	restoredSource, err := installedQwenPluginSource(root)
	if err != nil {
		return err
	}
	if !sameQwenPluginSource(restoredSource, source) {
		return fmt.Errorf("restored Qwen plugin source is %q, want %q", restoredSource, source)
	}
	return nil
}

func rollbackQwenPluginReplacement(executable, root, source, version, statePath string, profile qwenprofile.Identity) error {
	environment := os.Environ()
	if _, err := os.Lstat(root); err == nil {
		uninstall := qwenPluginUninstallCommand(executable, profile, environment)
		uninstall.Stdin, uninstall.Stdout, uninstall.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := uninstall.Run(); err != nil {
			return fmt.Errorf("remove failed replacement: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect failed replacement: %w", err)
	}
	restore, err := qwenPluginInstallCommand(executable, source, profile, environment)
	if err != nil {
		return err
	}
	restore.Stdin, restore.Stdout, restore.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := restore.Run(); err != nil {
		return fmt.Errorf("restore prior Qwen plugin: %w", err)
	}
	return verifyQwenPluginRollback(root, source, version, statePath)
}

//nolint:gocyclo // Exact plugin verification intentionally validates every manifest and inventory invariant together.
func verifyQwenPluginInstallation(root, expectedVersion string, enabled bool) error {
	if !enabled {
		return errors.New("qwen agent-sessions plugin is not enabled at user scope")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect Qwen plugin root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("qwen plugin root is not a real directory")
	}

	manifest, err := readQwenPluginObject(filepath.Join(root, "plugin.json"))
	if err != nil {
		return err
	}
	if stringValue(manifest["$schema"]) != qwenPluginSchema {
		return errors.New("qwen plugin manifest has the wrong schema")
	}
	if stringValue(manifest["name"]) != qwenPluginName {
		return errors.New("qwen plugin manifest has the wrong name")
	}
	if expectedVersion == "" || stringValue(manifest["version"]) != expectedVersion {
		return fmt.Errorf("qwen plugin manifest version is not %q", expectedVersion)
	}

	mcp, err := readQwenPluginObject(filepath.Join(root, "mcp.json"))
	if err != nil {
		return err
	}
	servers, ok := mcp["mcpServers"].(map[string]any)
	if stringValue(mcp["$schema"]) != qwenMCPSchema || !ok || len(servers) != 1 {
		return errors.New("qwen plugin MCP manifest has the wrong schema or inventory")
	}
	server, ok := servers[qwenMCPName].(map[string]any)
	arguments, argumentsOK := server["args"].([]any)
	if !ok || stringValue(server["type"]) != "stdio" || stringValue(server["command"]) != qwenMCPCommand ||
		!argumentsOK || len(arguments) != 1 || stringValue(arguments[0]) != "mcp" {
		return fmt.Errorf("qwen plugin does not provide the exact %s stdio MCP server", qwenMCPName)
	}
	entryInfo, err := os.Lstat(filepath.Join(root, "scripts", "native-entry"))
	if err != nil || !entryInfo.Mode().IsRegular() || entryInfo.Mode()&os.ModeSymlink != 0 || entryInfo.Mode().Perm()&0o111 == 0 {
		return errors.New("qwen plugin MCP native entry is not an executable regular file")
	}

	entries, err := os.ReadDir(filepath.Join(root, "skills"))
	if err != nil {
		return fmt.Errorf("inspect Qwen plugin skills: %w", err)
	}
	gotSkills := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			return fmt.Errorf("qwen plugin skills contain non-directory %q", entry.Name())
		}
		skillPath := filepath.Join(root, "skills", entry.Name(), "SKILL.md")
		skillInfo, statErr := os.Lstat(skillPath)
		if statErr != nil || !skillInfo.Mode().IsRegular() || skillInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("qwen plugin skills inventory entry %q has no regular direct-child SKILL.md", entry.Name())
		}
		gotSkills = append(gotSkills, entry.Name())
	}
	sort.Strings(gotSkills)
	if strings.Join(gotSkills, "\x00") != strings.Join(qwenPluginSkills, "\x00") {
		return fmt.Errorf("qwen plugin skills inventory is %q, want %q", gotSkills, qwenPluginSkills)
	}
	return nil
}

func readQwenPluginObject(path string) (map[string]any, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect Qwen plugin file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("qwen plugin file %s is not a regular file", path)
	}
	body, err := os.ReadFile(path) //nolint:gosec // Lstat above requires the exact selected-profile plugin file to be regular and non-symlink.
	if err != nil {
		return nil, fmt.Errorf("read Qwen plugin file %s: %w", path, err)
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, fmt.Errorf("decode Qwen plugin file %s: %w", path, err)
	}
	return value, nil
}

func qwenPluginEnabled(statePath string) (bool, error) {
	present, enabled, err := qwenPluginPolicy(statePath)
	if err != nil {
		return false, err
	}
	if !present {
		return false, errors.New("qwen extension store contains 0 agent-sessions policies")
	}
	return enabled, nil
}

func qwenPluginPolicy(statePath string) (bool, bool, error) {
	state, err := readQwenPluginObject(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, err
	}
	if intValue(state["version"]) != 2 {
		return false, false, errors.New("qwen extension store has an unsupported state version")
	}
	extensions, ok := state["extensions"].(map[string]any)
	if !ok {
		return false, false, errors.New("qwen extension store has no extensions inventory")
	}
	matches := 0
	enabled := false
	for _, raw := range extensions {
		policy, _ := raw.(map[string]any)
		if stringValue(policy["name"]) != qwenPluginName {
			continue
		}
		matches++
		enabled = stringValue(policy["defaultActivation"]) == "enabled"
	}
	if matches != 1 {
		if matches == 0 {
			return false, false, nil
		}
		return false, false, fmt.Errorf("qwen extension store contains %d agent-sessions policies", matches)
	}
	return true, enabled, nil
}

//nolint:gocyclo // Installation is one selected-profile native transaction with exact pre/post verification.
func runQwenPluginInstall(args []string) int {
	values, err := parseQwenPluginInstallArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: %v\n", err)
		return 2
	}
	profile, err := qwenprofile.Current()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: %v\n", err)
		return 1
	}
	if err := verifyQwenPluginInstallation(values.pluginRoot, values.version, true); err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: source payload verification failed: %v\n", err)
		return 1
	}
	home, err := qwenprofile.EffectiveHome(profile, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: %v\n", err)
		return 1
	}
	root := filepath.Join(home, "extensions", qwenPluginName)
	statePath := filepath.Join(home, "extension-store", "state.json")
	if enabled, stateErr := qwenPluginEnabled(statePath); stateErr == nil &&
		verifyQwenPluginInstallation(root, values.version, enabled) == nil {
		// Exact payload bytes are not enough for an idempotent installed release:
		// Qwen's native update follows the source recorded in its install metadata.
		// Reconcile a same-version developer install back to the immutable selected
		// INSTALL_ROOT instead of leaving future updates attached to a checkout.
		if source, sourceErr := installedQwenPluginSource(root); sourceErr == nil &&
			sameQwenPluginSource(source, values.pluginRoot) {
			return 0
		}
	}
	if err := refuseLiveQwenPluginMutation(profile); err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: %v\n", err)
		return 1
	}

	install, err := qwenPluginInstallCommand(values.qwen, values.pluginRoot, profile, os.Environ())
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: %v\n", err)
		return 1
	}
	command := install
	replacement := false
	rollbackSource := ""
	rollbackVersion := ""
	if _, statErr := os.Lstat(root); statErr == nil {
		// Qwen's native update follows the source recorded by the native
		// installer for a version change. It treats a same-version local source
		// as already current even when its payload changed, so exact drift at the
		// same version must use the native scoped uninstall/install transaction.
		source, sourceErr := installedQwenPluginSource(root)
		if sourceErr != nil {
			fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: %v\n", sourceErr)
			return 1
		}
		manifest, manifestErr := readQwenPluginObject(filepath.Join(root, "plugin.json"))
		sameVersion := manifestErr == nil && stringValue(manifest["version"]) == values.version
		if sameQwenPluginSource(source, values.pluginRoot) && !sameVersion {
			command = exec.Command(values.qwen, "extensions", "update", qwenPluginName) //nolint:gosec // Operator-selected Qwen executable and fixed native extension argv.
			command.Env = qwenprofile.ApplyEnvironment(os.Environ(), profile)
		} else {
			rollbackVersion = stringValue(manifest["version"])
			rollbackSource, err = qwenPluginRollbackSource(root, source, rollbackVersion)
			if err != nil {
				fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: refuse non-rollbackable replacement: %v\n", err)
				return 1
			}
			uninstall := qwenPluginUninstallCommand(values.qwen, profile, os.Environ())
			uninstall.Stdin, uninstall.Stdout, uninstall.Stderr = os.Stdin, os.Stdout, os.Stderr
			if uninstallErr := uninstall.Run(); uninstallErr != nil {
				fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: remove prior selected-profile plugin: %v\n", uninstallErr)
				return 1
			}
			replacement = true
		}
	} else if !os.IsNotExist(statErr) {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: inspect installed Qwen plugin: %v\n", statErr)
		return 1
	}
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if installErr := command.Run(); installErr != nil {
		if replacement {
			rollbackErr := rollbackQwenPluginReplacement(values.qwen, root, rollbackSource, rollbackVersion, statePath, profile)
			if rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: native Qwen installer failed: %v; rollback failed: %v\n", installErr, rollbackErr)
				return 1
			}
			fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: native Qwen installer failed: %v; prior plugin restored\n", installErr)
			return 1
		}
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: native Qwen installer failed: %v\n", installErr)
		return 1
	}
	enabled, err := qwenPluginEnabled(statePath)
	if err == nil {
		err = verifyQwenPluginInstallation(root, values.version, enabled)
	}
	if err != nil {
		if replacement {
			rollbackErr := rollbackQwenPluginReplacement(values.qwen, root, rollbackSource, rollbackVersion, statePath, profile)
			if rollbackErr != nil {
				fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: post-install verification failed: %v; rollback failed: %v\n", err, rollbackErr)
				return 1
			}
			fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: post-install verification failed: %v; prior plugin restored\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-install: post-install verification failed: %v\n", err)
		return 1
	}
	return 0
}

// runQwenPluginRemove removes only Agent Sessions' native Qwen extension from
// the exact selected profile. Native credentials, settings, other extensions,
// and transcripts remain Qwen-owned and are never inspected or rewritten.
func runQwenPluginRemove(args []string) int {
	qwen, err := parseQwenPluginRemoveArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-remove: %v\n", err)
		return 2
	}
	profile, err := qwenprofile.Current()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-remove: %v\n", err)
		return 1
	}
	home, err := qwenprofile.EffectiveHome(profile, os.LookupEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-remove: %v\n", err)
		return 1
	}
	root := filepath.Join(home, "extensions", qwenPluginName)
	statePath := filepath.Join(home, "extension-store", "state.json")
	present, _, stateErr := qwenPluginPolicy(statePath)
	_, rootErr := os.Lstat(root)
	if !present && errors.Is(rootErr, os.ErrNotExist) && stateErr == nil {
		return 0
	}
	if stateErr != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-remove: inspect selected-profile plugin state: %v\n", stateErr)
		return 1
	}
	if rootErr != nil && !errors.Is(rootErr, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-remove: inspect selected-profile plugin root: %v\n", rootErr)
		return 1
	}
	if err := refuseLiveQwenPluginMutation(profile); err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-remove: %v\n", err)
		return 1
	}
	command := exec.Command(qwen, "extensions", "uninstall", qwenPluginName) //nolint:gosec // Operator-selected Qwen executable and fixed native extension argv.
	command.Env = qwenprofile.ApplyEnvironment(os.Environ(), profile)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-remove: native Qwen uninstaller failed: %v\n", err)
		return 1
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-remove: post-remove plugin root remains: %v\n", err)
		return 1
	}
	if present, _, err := qwenPluginPolicy(statePath); err != nil || present {
		fmt.Fprintf(os.Stderr, "agent-session-runtime qwen-plugin-remove: post-remove policy remains or is unreadable: %v\n", err)
		return 1
	}
	return 0
}

// refuseLiveQwenPluginMutation scans the host process table rather than
// trusting stale registry files. A managed Qwen host or worker carries the
// product tag and exact presence-sensitive profile in its environment. Any
// unreadable matching Agent Sessions Qwen process fails closed.
func refuseLiveQwenPluginMutation(selected qwenprofile.Identity) error {
	processes, err := procinfo.List()
	if err != nil {
		return fmt.Errorf("cannot prove selected Qwen profile is idle: %w", err)
	}
	for _, process := range processes {
		environment, environmentErr := procinfo.Environment(process.PID)
		if environmentErr != nil || len(environment) == 0 {
			arguments, argumentsErr := procinfo.Args(process.PID)
			if argumentsErr == nil && looksLikeManagedQwenRuntime(arguments) {
				return fmt.Errorf("cannot inspect live managed Qwen process %d before plugin mutation", process.PID)
			}
			continue
		}
		if envutil.Value(environment, "AGENT_SESSIONS_PRODUCT") != "qwen" {
			continue
		}
		candidate, resolveErr := qwenprofile.ResolveEnvironment(envutil.Lookup(environment))
		if resolveErr != nil {
			return fmt.Errorf("cannot identify live managed Qwen process %d profile: %w", process.PID, resolveErr)
		}
		if candidate.Fingerprint == selected.Fingerprint {
			return fmt.Errorf("refuse Qwen plugin mutation while managed Qwen process %d uses the selected profile", process.PID)
		}
	}
	return nil
}

func looksLikeManagedQwenRuntime(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "qwen-host" || argument == "qwen-lane-manager" {
			return true
		}
	}
	return false
}

func installedQwenPluginSource(root string) (string, error) {
	metadata, err := readQwenPluginObject(filepath.Join(root, ".qwen-extension-install.json"))
	if err != nil {
		return "", fmt.Errorf("read native Qwen install metadata: %w", err)
	}
	source := stringValue(metadata["source"])
	if stringValue(metadata["type"]) != "local" || !filepath.IsAbs(source) {
		return "", errors.New("installed agent-sessions Qwen plugin does not have an absolute local source")
	}
	return source, nil
}

func sameQwenPluginSource(left, right string) bool {
	leftPath, leftErr := pathidentity.ExistingDirectory(left)
	rightPath, rightErr := pathidentity.ExistingDirectory(right)
	return leftErr == nil && rightErr == nil && leftPath == rightPath
}

type qwenPluginInstallArgs struct {
	qwen, pluginRoot, version string
}

func parseQwenPluginInstallArgs(args []string) (qwenPluginInstallArgs, error) {
	var result qwenPluginInstallArgs
	for index := 0; index < len(args); index++ {
		if index+1 >= len(args) {
			return result, errors.New("usage: qwen-plugin-install --qwen PATH --plugin-root PATH --version VERSION")
		}
		value := args[index+1]
		switch args[index] {
		case "--qwen":
			result.qwen = value
		case "--plugin-root":
			result.pluginRoot = value
		case "--version":
			result.version = value
		default:
			return result, fmt.Errorf("unknown option %q", args[index])
		}
		index++
	}
	if result.qwen == "" || result.pluginRoot == "" || result.version == "" {
		return result, errors.New("usage: qwen-plugin-install --qwen PATH --plugin-root PATH --version VERSION")
	}
	return result, nil
}

func parseQwenPluginRemoveArgs(args []string) (string, error) {
	if len(args) != 2 || args[0] != "--qwen" || strings.TrimSpace(args[1]) == "" {
		return "", errors.New("usage: qwen-plugin-remove --qwen PATH")
	}
	return args[1], nil
}
