// Package sessiontools owns product-neutral MCP/session helper contracts used
// by the live unified daemon. It deliberately does not own product transports,
// readiness, observers, or lifecycle state.
package sessiontools

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/antst/agent-sessions/internal/procinfo"
)

var ErrConnectorInactive = errors.New("connector is inactive")

// StdioMCPThreadID extracts the current thread claim supplied by the Codex MCP
// host on every tool invocation. Model arguments are deliberately ignored.
func StdioMCPThreadID(params json.RawMessage) (string, error) {
	var envelope map[string]any
	if json.Unmarshal(params, &envelope) != nil {
		return "", ErrConnectorInactive
	}
	meta, _ := envelope["_meta"].(map[string]any)
	threadID, ok := normalizeNativeID(stringValue(meta["threadId"]))
	if !ok {
		return "", ErrConnectorInactive
	}
	return threadID, nil
}

// CaptureNativeAncestry captures the exact caller and bounded parent chain.
func CaptureNativeAncestry(startPID, limit int) (procinfo.Identity, []procinfo.Identity, error) {
	if startPID <= 1 || limit <= 0 {
		return procinfo.Identity{}, nil, errors.New("native process identity is unavailable")
	}
	caller, err := procinfo.CaptureIdentity(startPID)
	if err != nil {
		return procinfo.Identity{}, nil, err
	}
	ancestry := make([]procinfo.Identity, 0, limit)
	seen := map[int]bool{startPID: true}
	pid := startPID
	for len(ancestry) < limit {
		info := procinfo.Read(pid)
		parent := info.Parent
		if info.Status != procinfo.Known || parent <= 1 || seen[parent] {
			break
		}
		seen[parent] = true
		identity, captureErr := procinfo.CaptureIdentity(parent)
		if captureErr != nil {
			return procinfo.Identity{}, nil, fmt.Errorf("capture native ancestor %d: %w", parent, captureErr)
		}
		ancestry = append(ancestry, identity)
		pid = parent
	}
	return caller, ancestry, nil
}

func validNativeID(value string) bool {
	_, ok := normalizeNativeID(value)
	return ok
}

// normalizeNativeID treats product-native session identifiers as bounded
// opaque values. The grammar preserves every original four-product identifier
// and admits the colon/dot forms used by server-backed products without
// allowing whitespace or envelope delimiters. Callers emit the normalized
// value rather than the untrimmed claim they validated.
func normalizeNativeID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return "", false
	}
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsNumber(character) || strings.ContainsRune("._:-", character) {
			continue
		}
		return "", false
	}
	return value, true
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
