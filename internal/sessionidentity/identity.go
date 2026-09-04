package sessionidentity

import (
	"strings"
	"unicode"
)

const NativeIDMaxBytes, NameMaxBytes, GroupMaxBytes, HostIDMaxBytes = 128, 256, 192, 128

func ValidNativeID(value string) bool { return bounded(value, NativeIDMaxBytes) }
func ValidName(value string) bool {
	return len(value) <= NameMaxBytes && !strings.ContainsFunc(value, unicode.IsControl)
}
func ValidGroup(value string) bool {
	if !strings.HasPrefix(value, "session:") {
		return bounded(value, GroupMaxBytes)
	}
	parts := strings.Split(strings.TrimPrefix(value, "session:"), "/")
	if len(value) > GroupMaxBytes || len(parts) < 2 || !ValidHostID(parts[0]) {
		return false
	}
	for _, nativeID := range parts[1:] {
		if !ValidNativeID(nativeID) {
			return false
		}
	}
	return true
}

func ValidHostID(value string) bool {
	return value != "" && len(value) <= HostIDMaxBytes && !strings.ContainsFunc(value, invalidHostRune)
}

func bounded(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) != "" && !strings.ContainsFunc(value, invalidBoundedRune)
}

func invalidHostRune(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !strings.ContainsRune("._-", r)
}
func invalidBoundedRune(r rune) bool { return r == '/' || unicode.IsControl(r) || unicode.IsSpace(r) }
