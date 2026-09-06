package daemon

import (
	"crypto/rand"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/antst/agent-sessions/bus/internal/protocol"
)

var hostPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validHost(value string) bool { return hostPattern.MatchString(value) }

func validIDPart(value string) bool {
	length := utf8.RuneCountInString(value)
	return length > 0 && length <= 128 && !strings.ContainsFunc(value, func(character rune) bool {
		return !unicode.IsPrint(character) || unicode.IsSpace(character)
	})
}

func validNamePart(value string) bool {
	length := utf8.RuneCountInString(value)
	return length > 0 && length <= 128 && !strings.ContainsFunc(value, func(character rune) bool {
		return !unicode.IsPrint(character)
	})
}

func qualify(value, host string) string { return value + "@" + host }

func unqualify(value string) string {
	if index := strings.LastIndexByte(value, '@'); index >= 0 {
		return value[:index]
	}
	return value
}

func canonicalSessionID(value, host string) (string, int) { return canonical(value, host, validIDPart) }
func canonicalName(value, host string) (string, int)      { return canonical(value, host, validNamePart) }

func canonical(value, host string, valid func(string) bool) (string, int) {
	if index := strings.LastIndexByte(value, '@'); index >= 0 {
		if !valid(value[:index]) {
			return "", protocol.InvalidFrame
		}
		if !validHost(value[index+1:]) || value[index+1:] != host {
			return "", protocol.UnknownHost
		}
		return value, 0
	}
	if !valid(value) {
		return "", protocol.InvalidFrame
	}
	return qualify(value, host), 0
}

func shares(left, right []string) bool {
	return slices.ContainsFunc(left, func(value string) bool { return slices.Contains(right, value) })
}

func orderedPeerGroups(declared []string, private string) []string {
	result := append([]string(nil), declared...)
	if !slices.Contains(result, private) {
		result = append(result, private)
	}
	return result
}

func lanePrivate(value row) string {
	if len(value.Groups) > 1 {
		return value.Groups[1]
	}
	return ""
}

func privateGroup(item *entry) string {
	if item.peer {
		return "session:" + item.row.SessionID
	}
	return lanePrivate(item.row)
}

func unsupported(open protocol.OpenOptions, supported []string) string {
	for _, field := range []struct {
		name string
		set  bool
	}{
		{"cwd", open.Cwd != ""},
		{"permission_mode", open.PermissionMode != ""},
		{"model", open.Model != ""},
		{"reasoning_effort", open.ReasoningEffort != ""},
		{"arguments", len(open.Arguments) != 0},
	} {
		if field.set && !slices.Contains(supported, field.name) {
			return field.name
		}
	}
	return ""
}

func randomID(prefix string) string {
	bytes := make([]byte, 12)
	_, _ = rand.Read(bytes)
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	for index := range bytes {
		bytes[index] = alphabet[int(bytes[index])%len(alphabet)]
	}
	return prefix + "-" + string(bytes)
}
