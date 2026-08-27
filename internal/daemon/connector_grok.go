package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

const grokConnectorName = "agent-sessions"

type grokConnectorDriver struct {
	runner         ConnectorCommandRunner
	executable     string
	userPluginRoot string
}

func newGrokConnectorDriver(options NativeConnectorOptions) ConnectorDriver {
	return &grokConnectorDriver{
		runner: options.Runner, executable: options.GrokExecutable, userPluginRoot: options.GrokUserPluginRoot,
	}
}

// Prepare implements ConnectorDriver.
func (driver *grokConnectorDriver) Prepare(ctx context.Context, request ConnectorRequest) (ConnectorMutation, error) {
	payloadRoot := filepath.Join(request.SourceRoot, "grok")
	if err := validateConnectorRequest(request, "grok", payloadRoot); err != nil {
		return nil, err
	}
	executable, available, err := resolveOptionalConnector(driver.runner, driver.executable)
	if err != nil || !available {
		if err != nil {
			return nil, err
		}
		return noConnectorMutation{}, nil
	}
	prior, err := inspectGrokConnector(ctx, driver.runner, executable, driver.userPluginRoot)
	if err != nil {
		return nil, err
	}
	snapshot, err := snapshotConnectorTree(driver.userPluginRoot)
	if err != nil {
		return nil, fmt.Errorf("snapshot prior Grok connector: %w", err)
	}
	mutation := &nativeConnectorMutation{}
	mutation.steps = append(mutation.steps,
		connectorStep{
			apply: func(context.Context) error { return replaceConnectorTree(payloadRoot, driver.userPluginRoot) },
			undo:  func(context.Context) error { return restoreConnectorTree(driver.userPluginRoot, snapshot) },
		},
		connectorStep{
			apply: func(ctx context.Context) error {
				if err := driver.runner.Run(ctx, executable, "plugin", "install", payloadRoot, "--trust"); err != nil {
					return err
				}
				if err := driver.runner.Run(ctx, executable, "plugin", "uninstall", grokConnectorName, "--keep-data"); err != nil {
					_ = driver.runner.Run(ctx, executable, "plugin", "uninstall", grokConnectorName)
					return err
				}
				return nil
			},
			undo: func(ctx context.Context) error {
				if prior.present && prior.enabled {
					return nil
				}
				if err := driver.runner.Run(ctx, executable, "plugin", "install", payloadRoot, "--trust"); err != nil {
					return err
				}
				return driver.runner.Run(ctx, executable, "plugin", "uninstall", grokConnectorName)
			},
		},
		connectorStep{apply: func(ctx context.Context) error {
			current, inspectErr := inspectGrokConnector(ctx, driver.runner, executable, driver.userPluginRoot)
			if inspectErr != nil {
				return inspectErr
			}
			if !current.present || !current.enabled {
				return errors.New("grok did not publish the exact replacement connector")
			}
			return nil
		}},
	)
	return mutation, nil
}

// Remove implements ConnectorDriver.
func (driver *grokConnectorDriver) Remove(ctx context.Context) error {
	executable, available, err := resolveOptionalConnector(driver.runner, driver.executable)
	if err != nil || !available {
		return err
	}
	prior, err := inspectGrokConnector(ctx, driver.runner, executable, driver.userPluginRoot)
	if err != nil {
		return err
	}
	if !prior.present {
		return nil
	}
	if err := driver.runner.Run(ctx, executable, "plugin", "install", driver.userPluginRoot, "--trust"); err != nil {
		return err
	}
	if err := driver.runner.Run(ctx, executable, "plugin", "uninstall", grokConnectorName); err != nil {
		return err
	}
	return removeExactConnectorTree(driver.userPluginRoot)
}

type grokConnectorState struct {
	present bool
	enabled bool
}

func inspectGrokConnector(ctx context.Context, runner ConnectorCommandRunner, executable, expectedRoot string) (grokConnectorState, error) {
	body, err := runner.Output(ctx, executable, "inspect", "--json")
	if err != nil {
		return grokConnectorState{}, err
	}
	inventory, err := decodeConnectorInventory(body)
	if err != nil {
		return grokConnectorState{}, err
	}
	pluginMatches, serverMatches := grokConnectorInventoryCounts(inventory)
	if pluginMatches == 0 && serverMatches == 0 {
		return grokConnectorState{}, nil
	}
	if pluginMatches != 1 || serverMatches != 1 {
		return grokConnectorState{}, fmt.Errorf(
			"grok connector inventory is partial or ambiguous: plugins=%d mcp_servers=%d", pluginMatches, serverMatches,
		)
	}
	if err := VerifyGrokConnectorInventory(body, expectedRoot); err != nil {
		return grokConnectorState{}, err
	}
	state := grokConnectorState{}
	for _, object := range connectorObjects(inventory) {
		if connectorString(object, "name") == grokConnectorName && connectorString(object, "scope") == "user" {
			path := connectorString(object, "path")
			canonical, pathErr := filepath.EvalSymlinks(path)
			wanted, wantedErr := filepath.EvalSymlinks(expectedRoot)
			if pathErr != nil || wantedErr != nil || canonical != wanted {
				return grokConnectorState{}, errors.New("grok agent-sessions plugin resolves outside the selected user plugin root")
			}
			state.present = true
			state.enabled, _ = object["enabled"].(bool)
		}
	}
	return state, nil
}

func grokConnectorInventoryCounts(inventory any) (int, int) {
	pluginMatches, serverMatches := 0, 0
	for _, object := range connectorObjects(inventory) {
		if connectorString(object, "name") == grokConnectorName && connectorString(object, "scope") == "user" {
			pluginMatches++
		}
		if connectorString(object, "name") == "agent_sessions" && connectorString(object, "transport") != "" {
			serverMatches++
		}
	}
	return pluginMatches, serverMatches
}

// VerifyGrokConnectorInventory verifies the exact enabled user plugin and MCP
// projection from bounded `grok inspect --json` metadata.
//
//nolint:gocyclo // Exact inventory validation keeps every plugin and MCP invariant in one shared gate.
func VerifyGrokConnectorInventory(body []byte, expectedRoot string) error {
	root, err := filepath.EvalSymlinks(expectedRoot)
	if err != nil {
		return fmt.Errorf("resolve expected user plugin root: %w", err)
	}
	wantedTarget, err := filepath.EvalSymlinks(filepath.Join(root, "scripts", "native-entry"))
	if err != nil {
		return fmt.Errorf("resolve expected native entry: %w", err)
	}
	decoded, err := decodeConnectorInventory(body)
	if err != nil {
		return err
	}
	inventory, ok := decoded.(map[string]any)
	if !ok {
		return errors.New("grok inspection is not an object")
	}
	plugins, _ := inventory["plugins"].([]any)
	pluginMatches := 0
	for _, raw := range plugins {
		plugin, _ := raw.(map[string]any)
		if connectorString(plugin, "name") != grokConnectorName {
			continue
		}
		pluginMatches++
		path, pathErr := filepath.EvalSymlinks(connectorString(plugin, "path"))
		enabled, enabledOK := plugin["enabled"].(bool)
		provides, _ := plugin["provides"].(map[string]any)
		if pathErr != nil || path != root || connectorString(plugin, "scope") != "user" ||
			!enabledOK || !enabled || connectorInteger(provides["skills"]) != 2 || connectorInteger(provides["mcpServers"]) != 1 {
			return errors.New("agent-sessions is not the enabled user plugin at the expected path")
		}
	}
	if pluginMatches != 1 {
		return fmt.Errorf("grok inspect returned %d agent-sessions plugin rows", pluginMatches)
	}
	servers, _ := inventory["mcpServers"].([]any)
	mcpMatches := 0
	for _, raw := range servers {
		server, _ := raw.(map[string]any)
		if connectorString(server, "name") != "agent_sessions" {
			continue
		}
		mcpMatches++
		target, targetErr := filepath.EvalSymlinks(connectorString(server, "target"))
		source, _ := server["source"].(map[string]any)
		sourcePath, sourceErr := filepath.EvalSymlinks(connectorString(source, "path"))
		if targetErr != nil || sourceErr != nil || target != wantedTarget || sourcePath != root ||
			connectorString(server, "transport") != "stdio" || connectorString(source, "type") != "plugin" ||
			connectorString(source, "plugin_name") != grokConnectorName {
			return errors.New("agent_sessions MCP is not sourced from the expected user plugin")
		}
	}
	if mcpMatches != 1 {
		return fmt.Errorf("grok inspect returned %d agent_sessions MCP rows", mcpMatches)
	}
	return nil
}

func connectorInteger(value any) int {
	switch typed := value.(type) {
	case json.Number:
		result, _ := strconv.Atoi(string(typed))
		return result
	case float64:
		return int(typed)
	case int:
		return typed
	default:
		return 0
	}
}

type connectorTreeEntry struct {
	path string
	mode fs.FileMode
	body []byte
}

type connectorTreeSnapshot struct {
	present bool
	entries []connectorTreeEntry
}

//nolint:gocyclo // Snapshot validation deliberately checks every filesystem type and resource bound together.
func snapshotConnectorTree(root string) (connectorTreeSnapshot, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return connectorTreeSnapshot{}, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return connectorTreeSnapshot{}, errors.New("connector payload root is not a real directory")
	}
	snapshot := connectorTreeSnapshot{present: true}
	total := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if len(snapshot.entries) >= 2048 {
			return errors.New("connector payload contains too many entries")
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("connector payload contains unsupported entry %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		item := connectorTreeEntry{path: relative, mode: info.Mode()}
		if info.Mode().IsRegular() {
			if info.Size() > 2*1024*1024 || total+int(info.Size()) > 16*1024*1024 {
				return errors.New("connector payload exceeds rollback snapshot bound")
			}
			item.body, err = os.ReadFile(path) //nolint:gosec // Exact non-secret connector payload below the validated user-plugin root.
			if err != nil {
				return err
			}
			total += len(item.body)
		}
		snapshot.entries = append(snapshot.entries, item)
		return nil
	})
	return snapshot, err
}

func replaceConnectorTree(source, destination string) error {
	snapshot, err := snapshotConnectorTree(source)
	if err != nil || !snapshot.present {
		return errors.New("replacement connector source is not a bounded real directory")
	}
	return restoreConnectorTree(destination, snapshot)
}

func restoreConnectorTree(root string, snapshot connectorTreeSnapshot) error {
	if err := removeExactConnectorTree(root); err != nil {
		return err
	}
	if !snapshot.present {
		return nil
	}
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".agent-sessions-connector-*")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = makeConnectorTreeWritable(stage)
			_ = os.RemoveAll(stage)
		}
	}()
	if err := materializeConnectorSnapshot(stage, snapshot); err != nil {
		return err
	}
	if err := os.Rename(stage, root); err != nil {
		return err
	}
	committed = true
	return nil
}

type stagedConnectorDirectory struct {
	path string
	mode os.FileMode
}

func materializeConnectorSnapshot(stage string, snapshot connectorTreeSnapshot) error {
	directories := make([]stagedConnectorDirectory, 0, len(snapshot.entries))
	rootMode := os.FileMode(0o700)
	for _, entry := range snapshot.entries {
		if entry.path == "." {
			rootMode = entry.mode.Perm()
			continue
		}
		path := filepath.Join(stage, entry.path)
		if !pathWithin(path, stage) {
			return errors.New("connector snapshot path escapes stage")
		}
		if entry.mode.IsDir() {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
			directories = append(directories, stagedConnectorDirectory{path: path, mode: entry.mode.Perm()})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, entry.body, entry.mode.Perm()); err != nil {
			return err
		}
	}
	// Apply immutable directory modes only after all children exist. Applying
	// a 0555 release mode during the pre-order walk would prevent creation of
	// that directory's own files on the same transaction.
	for index := len(directories) - 1; index >= 0; index-- {
		if err := os.Chmod(directories[index].path, directories[index].mode); err != nil {
			return err
		}
	}
	if err := os.Chmod(stage, rootMode); err != nil {
		return err
	}
	return nil
}

func removeExactConnectorTree(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refuse connector removal because payload root changed type")
	}
	if err := makeConnectorTreeWritable(root); err != nil {
		return err
	}
	return os.RemoveAll(root)
}

func makeConnectorTreeWritable(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("connector payload contains unsupported entry %s", path)
		}
		if info.IsDir() {
			directories = append(directories, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, directory := range directories {
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("connector directory changed type before removal")
		}
		if err := os.Chmod(directory, info.Mode().Perm()|0o700); err != nil {
			return err
		}
	}
	return nil
}
