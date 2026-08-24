// Package envutil provides deterministic operations on exec.Cmd-style
// environment slices. Product-specific environment policy belongs in the
// product adapter; this package owns only the shared NAME=value mechanics.
package envutil

import (
	"sort"
	"strings"
)

// Lookup returns a presence-sensitive lookup over environment. When a name is
// repeated, the last entry wins, matching the effective process environment.
func Lookup(environment []string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		prefix := name + "="
		for index := len(environment) - 1; index >= 0; index-- {
			if strings.HasPrefix(environment[index], prefix) {
				return strings.TrimPrefix(environment[index], prefix), true
			}
		}
		return "", false
	}
}

// Value returns the effective value of name, or an empty string when absent.
func Value(environment []string, name string) string {
	value, _ := Lookup(environment)(name)
	return value
}

// Replace removes every existing entry named by replacements and appends one
// value for each replacement. Appended keys are sorted for reproducibility.
func Replace(environment []string, replacements map[string]string) []string {
	result := make([]string, 0, len(environment)+len(replacements))
	for _, entry := range environment {
		name := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			name = entry[:separator]
		}
		if _, replaced := replacements[name]; !replaced {
			result = append(result, entry)
		}
	}
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+replacements[key])
	}
	return result
}

// Set replaces one environment value.
func Set(environment []string, name, value string) []string {
	return Replace(environment, map[string]string{name: value})
}
