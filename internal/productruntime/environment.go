package productruntime

import "strings"

// ParseNativeEnvironment converts transient NAME=value entries at the native
// process boundary without interpreting or defaulting their contents.
func ParseNativeEnvironment(environment []string) ([]EnvVar, error) {
	result := make([]EnvVar, 0, len(environment))
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" {
			return nil, ErrProtocol
		}
		result = append(result, EnvVar{Name: name, Value: value})
	}
	return result, nil
}
