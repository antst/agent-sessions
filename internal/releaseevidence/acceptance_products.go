package releaseevidence

import "strings"

// ProductsForAcceptanceCell returns the exact native products exercised by one
// expanded acceptance cell. It is shared by execution and result validation so
// a matrix-family spelling or role mapping cannot silently weaken either side.
func ProductsForAcceptanceCell(cell AcceptanceCell) []string {
	switch cell.Family {
	case "codex-interactive":
		return []string{"codex"}
	case "claude-interactive":
		return []string{"claude"}
	case "grok-interactive":
		return []string{"grok"}
	case "qwen-interactive":
		return []string{"qwen"}
	case "lane-lifecycle":
		return []string{"codex", "claude", "grok", "qwen"}
	case "parent-target-composition":
		parts := strings.Split(cell.ID, "-")
		if len(parts) == 3 {
			return uniqueProducts(parentTargetProduct(parts[1]), parentTargetProduct(parts[2]))
		}
	case "peer-lane-messaging":
		parts := strings.Split(cell.ID, "-")
		if len(parts) == 3 {
			return uniqueProducts(messagingRoleProduct(parts[1]), messagingRoleProduct(parts[2]))
		}
	case "archive-unarchive":
		parts := strings.Split(cell.ID, "-")
		if len(parts) == 2 {
			return uniqueProducts(parentTargetProduct(parts[1]))
		}
	}
	return nil
}

func parentTargetProduct(code string) string {
	switch code {
	case "C":
		return "codex"
	case "CL":
		return "claude"
	case "G":
		return "grok"
	case "Q":
		return "qwen"
	default:
		return ""
	}
}

func messagingRoleProduct(code string) string {
	switch code {
	case "CP", "CL":
		return "codex"
	case "CLP", "CLL":
		return "claude"
	case "GP", "GL":
		return "grok"
	case "QP", "QL":
		return "qwen"
	default:
		return ""
	}
}

func uniqueProducts(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
