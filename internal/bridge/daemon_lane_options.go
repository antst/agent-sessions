package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
	"github.com/antst/agent-sessions/internal/qwenprofile"
)

// NormalizeLaneCommand validates Codex options without starting native work.
func (adapter *codexDaemonAdapter) NormalizeLaneCommand(
	ctx context.Context,
	request daemonpkg.LaneCommandNormalizationRequest,
) (daemonpkg.LaneCommandNormalization, error) {
	if err := ctx.Err(); err != nil {
		return daemonpkg.LaneCommandNormalization{}, err
	}
	options, err := parseLaneArgs(append([]string{request.Command}, request.Arguments...))
	if err != nil {
		return daemonpkg.LaneCommandNormalization{}, err
	}
	options.cwd, options.name = request.Cwd, request.Name
	if options.schemaFile != "" {
		options.outputSchema, err = readLaneOutputSchema(options.schemaFile)
		if err != nil {
			return daemonpkg.LaneCommandNormalization{}, err
		}
	}
	result := daemonpkg.LaneCommandNormalization{
		Cwd: request.Cwd, PermissionMode: request.PermissionMode,
		NativeOptions: map[string]any{
			"model": options.model, "effort": options.effort, "sandbox": options.sandbox,
			"approval_policy": options.approvalPolicy, "config": append([]string(nil), options.configs...),
			"output_schema": options.outputSchema, "worktree": options.worktree,
		},
	}
	if options.web != nil {
		result.NativeOptions["web"] = *options.web
	}
	if options.worktree && (request.Command == "run" || request.Command == "start") {
		worktree, createErr := createLaneWorktree(resolveNativePaths(), request.Name, request.Cwd)
		if createErr != nil {
			return daemonpkg.LaneCommandNormalization{}, createErr
		}
		result.Cwd = worktree
		result.NativeOptions["original_cwd"], result.NativeOptions["worktree_path"] = request.Cwd, worktree
		result.Rollback = func() error { return removeLaneWorktree(request.Cwd, worktree) }
	}
	return result, nil
}

// NormalizeLaneCommand validates Claude options without starting native work.
func (adapter *claudeDaemonAdapter) NormalizeLaneCommand(
	ctx context.Context,
	request daemonpkg.LaneCommandNormalizationRequest,
) (daemonpkg.LaneCommandNormalization, error) {
	if err := ctx.Err(); err != nil {
		return daemonpkg.LaneCommandNormalization{}, err
	}
	options, err := parseClaudeLaneArgs(append([]string{request.Command}, request.Arguments...))
	if err != nil {
		return daemonpkg.LaneCommandNormalization{}, err
	}
	options.cwd, options.name = request.Cwd, request.Name
	if !options.permissionModeSet {
		options.permissionMode = request.PermissionMode
	}
	if options.schemaFile != "" {
		options.outputSchema, err = readLaneOutputSchema(options.schemaFile)
		if err != nil {
			return daemonpkg.LaneCommandNormalization{}, err
		}
	}
	result := daemonpkg.LaneCommandNormalization{
		Cwd: request.Cwd, PermissionMode: options.permissionMode,
		NativeOptions: map[string]any{
			"model": options.model, "effort": options.effort, "max_budget_usd": options.maxBudgetUSD,
			"tools": options.tools, "tools_set": options.toolsSet,
			"allowed_tools": options.allowedTools, "allowed_tools_set": options.allowedToolsSet,
			"disallowed_tools": options.disallowedTools, "disallowed_tools_set": options.disallowedToolsSet,
			"output_schema": options.outputSchema, "worktree": options.worktree,
		},
	}
	if options.worktree && (request.Command == "run" || request.Command == "start") {
		worktree, createErr := createLaneWorktree(resolveNativePaths(), request.Name, request.Cwd)
		if createErr != nil {
			return daemonpkg.LaneCommandNormalization{}, createErr
		}
		result.Cwd = worktree
		result.NativeOptions["original_cwd"], result.NativeOptions["worktree_path"] = request.Cwd, worktree
		result.Rollback = func() error { return removeLaneWorktree(request.Cwd, worktree) }
	}
	return result, nil
}

// NormalizeLaneCommand validates Grok options without starting native work.
func (adapter *grokDaemonAdapter) NormalizeLaneCommand(
	ctx context.Context,
	request daemonpkg.LaneCommandNormalizationRequest,
) (daemonpkg.LaneCommandNormalization, error) {
	if err := ctx.Err(); err != nil {
		return daemonpkg.LaneCommandNormalization{}, err
	}
	options, err := parseGrokLaneArgs(append([]string{request.Command}, request.Arguments...))
	if err != nil {
		return daemonpkg.LaneCommandNormalization{}, err
	}
	return daemonpkg.LaneCommandNormalization{
		Cwd: request.Cwd, PermissionMode: options.permissionMode,
		NativeOptions: map[string]any{"model": options.model, "reasoning_effort": options.reasoningEffort},
	}, nil
}

// NormalizeLaneCommand validates Qwen options without starting native work.
func (adapter *qwenDaemonAdapter) NormalizeLaneCommand(
	ctx context.Context,
	request daemonpkg.LaneCommandNormalizationRequest,
) (daemonpkg.LaneCommandNormalization, error) {
	if err := ctx.Err(); err != nil {
		return daemonpkg.LaneCommandNormalization{}, err
	}
	options, err := parseQwenLaneArgs(append([]string{request.Command}, request.Arguments...))
	if err != nil {
		return daemonpkg.LaneCommandNormalization{}, err
	}
	profile, err := resolveQwenLaneProfile(options)
	if err != nil {
		return daemonpkg.LaneCommandNormalization{}, err
	}
	if request.Command == "resume" && stringValue(request.NativeActor["profile"]) != "" {
		stored, storedErr := qwenDaemonProfile(request.NativeActor)
		if storedErr != nil {
			return daemonpkg.LaneCommandNormalization{}, storedErr
		}
		if matchErr := qwenprofile.MatchResume(stored, profile); matchErr != nil {
			return daemonpkg.LaneCommandNormalization{}, matchErr
		}
	}
	permission := request.PermissionMode
	if options.permissionModeSet {
		permission = options.permissionMode
	}
	return daemonpkg.LaneCommandNormalization{
		Cwd: request.Cwd, PermissionMode: permission,
		NativeOptions: map[string]any{
			"profile": profile.Fingerprint, "qwen_home_set": profile.QwenHomeSet, "qwen_home": profile.QwenHome,
			"qwen_runtime_dir_set": profile.QwenRuntimeSet, "qwen_runtime_dir": profile.QwenRuntimeDir,
			"launch_preference": options.launchPreference,
		},
	}, nil
}

func daemonLaneNativeOptions(turn daemonpkg.LaneTurnRecord) map[string]any {
	options := mapValue(turn.InputReference["options"])
	return mapValue(options["native"])
}

func daemonLaneStringSlice(value any) []string {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func daemonLaneRawJSON(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	if raw, ok := value.(json.RawMessage); ok {
		return append(json.RawMessage(nil), raw...), nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if string(body) == "null" || string(body) == `""` {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode normalized lane JSON: %w", err)
	}
	return body, nil
}

func cleanupDaemonLaneWorktree(lane daemonpkg.LaneRecord) error {
	worktree := strings.TrimSpace(stringValue(lane.NativeActor["worktree_path"]))
	if worktree == "" {
		return nil
	}
	original := strings.TrimSpace(stringValue(lane.NativeActor["original_cwd"]))
	if original == "" {
		return errors.New("daemon lane worktree lacks its original cwd")
	}
	return removeLaneWorktree(original, worktree)
}
