package sessiontools

import (
	"strings"
	"unicode"
)

// NormalizePeerName applies the stable public peer-address normalization.
func NormalizePeerName(value string) string {
	var output []rune
	separator := false
	for _, character := range strings.TrimSpace(value) {
		if unicode.IsLetter(character) || unicode.IsNumber(character) || character == '.' || character == '_' || character == '-' {
			output = append(output, character)
			separator = false
		} else if !separator {
			output = append(output, '-')
			separator = true
		}
		if len(output) >= 80 {
			break
		}
	}
	result := strings.Trim(string(output), "._-")
	if result == "" {
		return "codex"
	}
	return result
}
