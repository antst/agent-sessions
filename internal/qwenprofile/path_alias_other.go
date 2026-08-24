//go:build !darwin

package qwenprofile

func resolvePlatformPathAlias(string) (string, bool, error) {
	return "", false, nil
}
