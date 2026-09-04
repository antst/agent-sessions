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
	hostID, nativeID, separated := strings.Cut(strings.TrimPrefix(value, "session:"), "/")
	return len(value) <= GroupMaxBytes && separated && ValidHostID(hostID) && ValidNativeID(nativeID)
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
