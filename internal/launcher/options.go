package launcher

import (
	"strings"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

// productOptionTable contains native value arity only. It never assigns
// meaning to vendor options and never removes or rewrites them; it protects a
// native option's following value from being mistaken for a wrapper option.
type productOptionTable struct {
	valueOptions  map[string]struct{}
	attachedShort []string
}

var productOptionTables = map[string]productOptionTable{
	"codex": newProductOptionTable(
		[]string{
			"--config", "--enable", "--disable", "--remote", "--remote-auth-token-env", "--image",
			"--model", "--local-provider", "--profile", "--sandbox", "--cd", "--add-dir", "--ask-for-approval",
			"-c", "-i", "-m", "-p", "-s", "-C", "-a",
		},
		[]string{"-c", "-i", "-m", "-p", "-s", "-C", "-a"},
	),
	"claude": newProductOptionTable(
		[]string{
			"--add-dir", "--agent", "--agents", "--allowedTools", "--append-system-prompt", "--betas",
			"--debug-file", "--disallowedTools", "--effort", "--fallback-model", "--ide", "--input-format",
			"--json-schema", "--max-budget-usd", "--max-turns", "--mcp-config", "--model", "--name",
			"--output-format", "--permission-mode", "--permission-prompt-tool", "--plugin-dir", "--resume", "-r",
			"--session-id", "--settings", "--system-prompt", "--tools",
		},
		nil,
	),
	"grok": newProductOptionTable(
		[]string{
			"--agent", "--agents", "--allow", "--allowedTools", "--cwd", "--debug-file", "--deny", "--disallowedTools",
			"--disallowed-tools", "--json-schema", "--model", "--output-format", "--permission-mode", "--prompt-file",
			"--prompt-json", "--reasoning-effort", "--effort", "--rules", "--session-id", "--sandbox", "--single",
			"--system-prompt-override", "--system-prompt", "--tools", "--worktree-ref", "--ref", "--peer-name", "-n",
			"--compaction-mode", "--compaction-detail", "-m", "-p", "-s",
		},
		[]string{"-m", "-p", "-s"},
	),
	"qwen": newProductOptionTable(
		[]string{
			"-m", "--model", "--fallback-model", "-p", "--prompt", "-i", "--prompt-interactive",
			"-o", "--output-format", "-r", "--resume", "--approval-mode", "--session-id", "--json-file",
			"--json-fd", "--input-file", "--mcp-config", "--include-directories", "--allowed-mcp-server-names",
			"--theme", "-n", "--name", "--qwen-home", "--runtime-dir", "--state-dir",
		},
		nil,
	),
}

func init() {
	for _, descriptor := range productcatalog.RuntimeInventory() {
		if len(descriptor.NativeValueOptions) == 0 {
			continue
		}
		productOptionTables[descriptor.ID] = newProductOptionTable(descriptor.NativeValueOptions, descriptor.NativeAttachedShort)
	}
}

func newProductOptionTable(values, attachedShort []string) productOptionTable {
	table := productOptionTable{
		valueOptions:  make(map[string]struct{}, len(values)),
		attachedShort: append([]string(nil), attachedShort...),
	}
	for _, value := range values {
		table.valueOptions[value] = struct{}{}
	}
	return table
}

// productOptionConsumesNext reports native option arity without interpreting
// that option. Attached values never consume the following argument.
func productOptionConsumesNext(product, argument string) bool {
	table, ok := productOptionTables[product]
	if !ok || strings.Contains(argument, "=") {
		return false
	}
	if _, ok := table.valueOptions[argument]; ok {
		return true
	}
	for _, prefix := range table.attachedShort {
		if strings.HasPrefix(argument, prefix) && argument != prefix {
			return false
		}
	}
	return false
}

// scanPeerWrapperOptions extracts only the product-neutral wrapper layer.
// Every unowned byte remains in caller order, and `--` ends wrapper scanning.
func scanPeerWrapperOptions(product string, args []string) ([]string, peerLaunchContext, error) {
	if _, ok := productOptionTables[product]; !ok {
		return nil, peerLaunchContext{}, usageError("unsupported product wrapper: " + product)
	}
	return scanPeerWrapperOptionsWithArity(args, func(argument string) bool {
		return productOptionConsumesNext(product, argument)
	})
}

func scanPeerWrapperOptionsWithArity(args []string, consumesNext func(string) bool) ([]string, peerLaunchContext, error) {
	forwarded := make([]string, 0, len(args))
	var context peerLaunchContext
	remaining := args
	for len(remaining) > 0 {
		argument := remaining[0]
		remaining = remaining[1:]
		if argument == "--" {
			forwarded = append(forwarded, argument)
			forwarded = append(forwarded, remaining...)
			break
		}
		switch {
		case argument == "-g", argument == "--group":
			if len(remaining) == 0 {
				return nil, peerLaunchContext{}, usageError("-g/--group requires a non-empty value")
			}
			group := remaining[0]
			remaining = remaining[1:]
			if strings.TrimSpace(group) == "" {
				return nil, peerLaunchContext{}, usageError("-g/--group requires a non-empty value")
			}
			context.groups = append(context.groups, group)
			context.groupsSpecified = true
		case strings.HasPrefix(argument, "-g="), strings.HasPrefix(argument, "--group="):
			_, value, _ := strings.Cut(argument, "=")
			if strings.TrimSpace(value) == "" {
				return nil, peerLaunchContext{}, usageError("-g/--group requires a non-empty value")
			}
			context.groups = append(context.groups, value)
			context.groupsSpecified = true
		case argument == "--parent-session" || strings.HasPrefix(argument, "--parent-session="):
			return nil, peerLaunchContext{}, usageError("--parent-session is internal; parent membership is assigned by an attested lane launch")
		case argument == "--inherit-groups":
			context.inheritParentGroups, context.inheritGroupsSpecified = true, true
		case argument == "--no-inherit-groups":
			context.inheritParentGroups, context.inheritGroupsSpecified = false, true
		case argument == "--no-yolo":
			context.forceNoYolo = true
		default:
			forwarded = append(forwarded, argument)
			if consumesNext(argument) && len(remaining) > 0 {
				forwarded = append(forwarded, remaining[0])
				remaining = remaining[1:]
			}
		}
	}
	return forwarded, context, nil
}
