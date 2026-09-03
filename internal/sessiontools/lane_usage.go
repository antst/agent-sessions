package sessiontools

import (
	"fmt"
	"strings"

	"github.com/antst/agent-sessions/internal/productcatalog"
)

// LaneUsage returns stable original-four help from a catalog-validated product.
// New product help is added with its descriptor/driver during central composition.
func LaneUsage(product string) (string, error) {
	if _, ok := productcatalog.ByID(product); !ok {
		return "", fmt.Errorf("lane product %q is unsupported", product)
	}
	usage, ok := laneUsageByProduct[product]
	if !ok {
		return "", fmt.Errorf("lane help for product %q is unavailable", product)
	}
	return strings.TrimLeft(usage, "\n"), nil
}

var laneUsageByProduct = map[string]string{
	"codex": `codex-peer-lane — named, messageable Codex lanes on the shared App Server

Usage:
  codex-peer-lane run   --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  codex-peer-lane start --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  codex-peer-lane resume THREAD_OR_NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  codex-peer-lane wait THREAD_OR_NAME [--timeout SECONDS]
  codex-peer-lane status THREAD_OR_NAME
  codex-peer-lane interrupt THREAD_OR_NAME
  codex-peer-lane archive THREAD_OR_NAME
  codex-peer-lane list [--all] [--mine]
  codex-peer-lane doctor [--json]

Options include --name, --cd, --model, --effort/--reasoning-effort, --sandbox, --approval-policy, --yolo, --no-yolo,
--timeout, --persistent, --auto-archive-after, --no-auto-archive, --schema,
--worktree, --group GROUP, --inherit-groups, --no-inherit-groups, and --mine.
Headless lanes cannot answer approval prompts; the wrapper never widens policy.
`,
	"claude": `claude-peer-lane — named, messageable Claude Code lanes

Usage:
  claude-peer-lane run   --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  claude-peer-lane start --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  claude-peer-lane resume SESSION_OR_NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  claude-peer-lane wait SESSION_OR_NAME [--timeout SECONDS]
  claude-peer-lane status SESSION_OR_NAME
  claude-peer-lane interrupt SESSION_OR_NAME
  claude-peer-lane archive SESSION_OR_NAME
  claude-peer-lane list [--all] [--mine]
  claude-peer-lane doctor [--json]

Options include --name, --cd, --model, --agent, --effort/--reasoning-effort, --permission-mode, --yolo, --no-yolo,
--max-budget-usd, --tools, --allowed-tools, --disallowed-tools, --schema, --bare,
--timeout, --persistent, --auto-archive-after, --no-auto-archive, --worktree,
--group GROUP, --inherit-groups, --no-inherit-groups, and --mine.
`,
	"grok": `grok-peer-lane — named, messageable Grok Build lanes

Usage:
  grok-peer-lane run   --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  grok-peer-lane start --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  grok-peer-lane resume SESSION_OR_NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  grok-peer-lane wait SESSION_OR_NAME [--timeout SECONDS]
  grok-peer-lane status SESSION_OR_NAME
  grok-peer-lane interrupt SESSION_OR_NAME
  grok-peer-lane archive SESSION_OR_NAME
  grok-peer-lane list [--all] [--mine]
  grok-peer-lane doctor [--json]

Options include --name, --cd, --model, --agent, --effort/--reasoning-effort, --yolo, --no-yolo,
--permission-mode bypassPermissions, --timeout, --persistent,
--auto-archive-after, --no-auto-archive, --group GROUP, --inherit-groups,
--no-inherit-groups, and --mine.
`,
	"qwen": `qwen-peer-lane — named, messageable Qwen Code lanes

Usage:
  qwen-peer-lane run   --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  qwen-peer-lane start --name NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  qwen-peer-lane resume SESSION_OR_NAME [OPTIONS] [--prompt-file FILE] < prompt.md
  qwen-peer-lane wait SESSION_OR_NAME [--timeout SECONDS]
  qwen-peer-lane status SESSION_OR_NAME
  qwen-peer-lane interrupt SESSION_OR_NAME
  qwen-peer-lane archive SESSION_OR_NAME
  qwen-peer-lane list [--all] [--mine]
  qwen-peer-lane doctor [--json]

Options include --name, --cwd, --group GROUP, --inherit-groups,
--no-inherit-groups, --qwen-home, --yolo, --no-yolo, --approval-mode,
--timeout, --persistent, --auto-archive-after, --no-auto-archive, and --mine.
`,
}
