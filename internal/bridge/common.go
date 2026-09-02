package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/antst/agent-sessions/internal/sessionkey"
	"github.com/antst/agent-sessions/internal/sessiontools"
)

func sessionKey(id string) string { return sessionkey.FromID(id) }

func randomID() string {
	body := make([]byte, 16)
	_, _ = rand.Read(body)
	body[6] = (body[6] & 0x0f) | 0x40
	body[8] = (body[8] & 0x3f) | 0x80
	s := hex.EncodeToString(body)
	return fmt.Sprintf("%s-%s-%s-%s-%s", s[:8], s[8:12], s[12:16], s[16:20], s[20:])
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func stringValue(value any) string { text, _ := value.(string); return text }
func boolValue(value any) bool     { result, _ := value.(bool); return result }

func stringSlice(value any) []string {
	if values, ok := value.([]string); ok {
		return append([]string(nil), values...)
	}
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func intValue(value any) int {
	switch current := value.(type) {
	case int:
		return current
	case float64:
		return int(current)
	case interface{ Int64() (int64, error) }:
		parsed, _ := current.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func putIf(target map[string]any, key, value string) {
	if strings.TrimSpace(value) != "" {
		target[key] = value
	}
}

func first8(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

func sanitizeName(value string) string {
	name := sessiontools.NormalizePeerName(value)
	if name == "" {
		return "session"
	}
	return name
}

func canonicalBase(path string) string { return filepath.Base(filepath.Clean(path)) }

func samePath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	left, leftErr := filepath.Abs(left)
	right, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(left) == filepath.Clean(right)
}

func parentProcessID(pid int) (int, bool) { return parentProcessIDNative(pid) }

func processHasAncestor(start, ancestor int) bool {
	visited := map[int]bool{}
	for pid := start; pid > 1 && !visited[pid]; {
		if pid == ancestor {
			return true
		}
		visited[pid] = true
		next, ok := parentProcessID(pid)
		if !ok {
			return false
		}
		pid = next
	}
	return false
}

const maxFrameBytes = 1024 * 1024

func mapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func int64Value(value any) int64 {
	switch current := value.(type) {
	case int64:
		return current
	case int:
		return int64(current)
	case float64:
		return int64(current)
	case interface{ Int64() (int64, error) }:
		parsed, _ := current.Int64()
		return parsed
	default:
		return 0
	}
}

func assignDottedNative(target map[string]any, key string, value any) error {
	parts := strings.FieldsFunc(key, func(r rune) bool { return r == '.' })
	if len(parts) == 0 {
		return errors.New("config key must not be empty")
	}
	cursor := target
	for _, part := range parts[:len(parts)-1] {
		next, ok := cursor[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			cursor[part] = next
		}
		cursor = next
	}
	cursor[parts[len(parts)-1]] = value
	return nil
}
